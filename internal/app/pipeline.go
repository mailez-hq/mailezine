package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"time"

	"mailezine/internal/auth"
	"mailezine/internal/config"
	"mailezine/internal/delivery"
	"mailezine/internal/directory"
	"mailezine/internal/dkim"
	"mailezine/internal/maildns"
	"mailezine/internal/mailmtasts"
	"mailezine/internal/queue"
)

// wireQueue builds the outbound queue with opportunistic DKIM signing,
// metrics, bounce and delay-warning routing under runCtx (the per-term
// context when HA is enabled, so workers stop before the KV does).
func (a *App) wireQueue(runCtx context.Context) error {
	if !a.cfg.Outbound.Enabled {
		return nil
	}
	dnsResolver := maildns.NewSystemResolver()
	directDeliverer := &queue.SMTPDeliverer{
		Logger:    a.logger,
		Hostname:  a.cfg.Hostname,
		Resolver:  net.DefaultResolver,
		Port:      a.cfg.Outbound.Port,
		FixedHost: a.cfg.Outbound.FixedHost,
		FixedPort: a.cfg.Outbound.FixedPort,
		Username:  a.cfg.Outbound.SmarthostUsername,
		Password:  a.cfg.Outbound.SmarthostPassword,
		// MTA-STS/DANE policy for outbound TLS (opportunistic fallback).
		PolicyResolver: dnsResolver,
		MTSTS:          mailmtasts.NewFetcher(),
	}
	deliverer := &queue.RelayDeliverer{
		Directory: a.dir,
		Direct:    directDeliverer,
		Logger:    a.logger,
	}
	qOpts := queue.DefaultOptions()
	q := a.cfg.Queue
	if q.MaxAttempts > 0 {
		qOpts.MaxAttempts = q.MaxAttempts
	}
	if q.BaseRetry > 0 {
		qOpts.BaseRetry = q.BaseRetry
	}
	if q.MaxRetry > 0 {
		qOpts.MaxRetry = q.MaxRetry
	}
	if q.PollInterval > 0 {
		qOpts.PollInterval = q.PollInterval
	}
	if q.DelayWarning > 0 {
		qOpts.DelayWarning = q.DelayWarning
	}
	a.qm = queue.New(a.st.kv, a.st.blob, deliverer, qOpts, a.logger)
	a.qm.SetSigner(opportunisticSigner{dkim.NewSigner(a.cfg.DKIMVaultURL, a.logger, a.cfg.StackSecret), a.logger})
	a.qm.SetOnEvent(func(event string) {
		a.m.QueueMessages.WithLabelValues(event).Inc()
	})
	a.qm.SetMetrics(a.m, runCtx)
	a.qm.SetBounceHandler(func(ctx context.Context, from string, msg *queue.Message, _ []byte, failures []queue.BounceFailure) {
		dsnBytes, derr := queue.ComposeBounceDSN(from, msg, failures, a.cfg.Hostname)
		a.routeDSN(ctx, from, dsnBytes, derr, "Delivery Status Notification (Failure)")
	})
	a.qm.SetDelayWarningHandler(func(ctx context.Context, from string, msg *queue.Message, _ []byte, waited time.Duration) {
		dsnBytes, derr := queue.ComposeDelayDSN(from, msg, waited, a.cfg.Hostname)
		a.routeDSN(ctx, from, dsnBytes, derr, "Delayed Mail Notification")
	})
	a.qmDone = make(chan struct{})
	go func() {
		defer close(a.qmDone)
		if err := a.qm.Run(runCtx); err != nil {
			a.logger.Error("queue", "err", err)
		}
	}()
	a.logger.Info("outbound queue", "port", a.cfg.Outbound.Port, "dkimVault", a.cfg.DKIMVaultURL)
	a.pipeline.Redirect = func(ctx context.Context, from, to string, data []byte) error {
		// Sieve redirect: rewrite the envelope sender like relayed mail so
		// bounces route back through us.
		data = delivery.Outclean(data)
		relayFrom := from
		if rewritten, err := a.dir.SRSForward(ctx, from); err == nil && rewritten != "" {
			relayFrom = rewritten
		}
		if _, err := a.qm.Submit(ctx, relayFrom, []string{to}, subjectOf(data), bytes.NewReader(data)); err != nil {
			return err
		}
		a.logger.Info("sieve redirect queued", "from", relayFrom, "to", to, "bytes", len(data))
		return nil
	}
	return nil
}

// routeDSN routes a composed notification: local recipients get it through
// the delivery pipeline; external recipients are spooled with a null
// envelope sender (which can never bounce again, RFC 5321 §6.1).
func (a *App) routeDSN(ctx context.Context, from string, dsnBytes []byte, derr error, subject string) {
	if from == "" {
		return
	}
	if derr != nil || len(dsnBytes) == 0 {
		if derr != nil {
			a.logger.Error("dsn: compose", "from", from, "err", derr)
		}
		return
	}
	if _, err := a.dir.Aliases(ctx, from); err == nil {
		if err := a.pipeline.Deliver(ctx, nil, "", []string{from}, dsnBytes); err != nil {
			a.logger.Error("dsn: local deliver", "from", from, "err", err)
		}
		return
	}
	if _, err := a.qm.Submit(ctx, "", []string{from}, subject, bytes.NewReader(dsnBytes)); err != nil {
		a.logger.Error("dsn: queue", "from", from, "err", err)
	}
}

// opportunisticSigner degrades a DKIM signing failure to unsigned delivery
// (signing is best-effort: a vault outage must not stop mail).
type opportunisticSigner struct {
	s      *dkim.Signer
	logger *slog.Logger
}

func (o opportunisticSigner) Sign(ctx context.Context, from string, msg []byte) ([]byte, error) {
	signed, err := o.s.Sign(ctx, from, msg)
	if err != nil {
		o.logger.Warn("dkim: sign failed; sending unsigned", "from", from, "err", err)
		return msg, nil
	}
	return signed, nil
}

func newDirectory(cfg config.Config, logger *slog.Logger) (directory.Service, error) {
	switch cfg.Directory.Mode {
	case "dev":
		d, err := directory.LoadDevFile(cfg.Directory.File)
		if err != nil {
			return nil, err
		}
		logger.Info("directory", "mode", "dev", "file", cfg.Directory.File)
		return d, nil
	case "mailez":
		base := "http://" + cfg.BackendAddress + "/stack/directory"
		logger.Info("directory", "mode", "mailez", "base", base, "cacheTTL", cfg.Directory.CacheTTL)
		return directory.NewMailez(base, cfg.Directory.CacheTTL, cfg.MetaCacheSizeBytes), nil
	default:
		return nil, errors.New("config: unknown directory mode " + cfg.Directory.Mode)
	}
}

func newAuth(cfg config.Config, logger *slog.Logger) (auth.Service, error) {
	switch cfg.Auth.Mode {
	case "dev":
		s, err := auth.LoadDevFile(cfg.Auth.DevPasswordsFile)
		if err != nil {
			return nil, err
		}
		logger.Info("auth", "mode", "dev", "file", cfg.Auth.DevPasswordsFile)
		return s, nil
	case "mailez":
		base := "http://" + cfg.BackendAddress + "/stack"
		s := auth.NewMailez(base, cfg.StackSecret)
		logger.Info("auth", "mode", "mailez", "base", base)
		return s, nil
	default:
		return nil, errors.New("config: unknown auth mode " + cfg.Auth.Mode)
	}
}
