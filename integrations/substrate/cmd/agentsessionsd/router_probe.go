package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"github.com/agent-primitives/sessions/integrations/substrate/routertls"
)

// routerProbeTimeout bounds each attempt of the startup router probe.
var routerProbeTimeout = 10 * time.Second

// probeBackoff spaces the attempts of the startup router probe while the router is unreachable.
type probeBackoff struct {
	initial, max time.Duration
	attempts     int
}

// routerProbeBackoff retries an unreachable router for about five minutes (1s doubling to 30s, 15
// attempts), so a daemon that starts before the router still logs the router's answer.
var routerProbeBackoff = probeBackoff{initial: time.Second, max: 30 * time.Second, attempts: 15}

// http2Preface is an HTTP/2 client connection preface followed by an empty SETTINGS frame. A router
// that admitted the client answers with its own SETTINGS frame.
var http2Preface = []byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n\x00\x00\x00\x04\x00\x00\x00\x00\x00")

// probeOutcome classifies one router probe.
type probeOutcome int

const (
	probeAccepted probeOutcome = iota
	probeRefused
	probeUnverified
	probeFailed
)

// probeRouter opens one TLS connection to the router with the router client's current
// certificate, sends an HTTP/2 preface and reads the router's first frame. It names no actor, so it
// wakes nothing. Under TLS 1.3 the router checks the client certificate and its SPIFFE ID after the
// client's handshake completes, so a refusal arrives as an alert on the first read: v0.6.0's envoy
// router sends "certificate required" to a client without a certificate and "unknown certificate"
// to one whose SPIFFE ID is not on its allowlist.
func probeRouter(ctx context.Context, creds *routertls.Credentials, addr string) (probeOutcome, error) {
	cfg, err := creds.TLSConfig()
	if err != nil {
		return probeFailed, err
	}
	cfg.NextProtos = []string{"h2"}
	d := tls.Dialer{Config: cfg}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return classifyTLSError(err)
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	// Cancelling ctx stops the dial but not the write and read below: a router that completes the
	// handshake and then sends nothing would hold the probe, and with it shutdown, until the deadline.
	// A past deadline unblocks both. Registered after the deadline above, so an already-cancelled
	// ctx wins.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	// A write can fail (broken pipe) when the router's alert has already arrived; the read below
	// still returns the alert, which names the cause.
	_, werr := conn.Write(http2Preface)
	var frame [9]byte
	if _, err := io.ReadFull(conn, frame[:]); err != nil {
		if werr != nil {
			if outcome, rerr := classifyTLSError(err); outcome == probeRefused {
				return outcome, rerr
			}
			return classifyTLSError(werr)
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return probeRefused, fmt.Errorf("router closed the connection after the handshake: %w", err)
		}
		return classifyTLSError(err)
	}
	return probeAccepted, nil
}

// classifyTLSError separates a router that refused this client (a TLS alert from the peer) and a
// router this client could not verify from other failures (the router is unreachable).
func classifyTLSError(err error) (probeOutcome, error) {
	var verify *tls.CertificateVerificationError
	if errors.As(err, &verify) {
		return probeUnverified, err
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "remote error" {
		return probeRefused, err
	}
	return probeFailed, err
}

// startRouterProbe probes the router in the background and logs its answer, so an operator sees an
// allowlist or trust mistake at startup rather than on the first session. It never gates startup
// or readiness. stop cancels the probe and waits for it.
func (f *substrateFlags) startRouterProbe(logger *slog.Logger) (stop func()) {
	if logger == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runRouterProbe(ctx, logger, f.routerCreds, *f.router, *f.routerCA, routerProbeBackoff)
	}()
	return func() {
		cancel()
		<-done
	}
}

// runRouterProbe probes the router until it answers, logging each attempt that could not reach it
// and waiting b between attempts, and logs the answer once. An answer ends the probe, refusals
// included: the router's allowlist and serving certificate are set at install, so asking again
// would repeat the same answer. A router that is still rolling out can answer with a refusal (it
// closes the connection after the handshake) or a certificate the CA file does not yet trust; the
// probe logs that once and sessions still dial the router on every call. After b.attempts
// unreachable attempts it gives up. Cancelling ctx stops it without logging.
func runRouterProbe(ctx context.Context, logger *slog.Logger, creds *routertls.Credentials, router, caFile string, b probeBackoff) {
	wait := b.initial
	for attempt := 1; ; attempt++ {
		actx, cancel := context.WithTimeout(ctx, routerProbeTimeout)
		outcome, err := probeRouter(actx, creds, router)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if outcome != probeFailed {
			logRouterProbe(logger, outcome, err, creds, router, caFile)
			return
		}
		if attempt >= b.attempts {
			logger.Warn("atenet-router probe gave up; sessions still dial the router on every call", "atenet_router", router, "client_id", creds.SPIFFEID(), "attempts", attempt, "error", err)
			return
		}
		logRouterProbe(logger, outcome, err, creds, router, caFile, "attempt", attempt, "retry_in", wait)
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		wait = min(2*wait, b.max)
	}
}

// logRouterProbe logs one probe outcome; attrs are added to an unreachable router's line.
func logRouterProbe(logger *slog.Logger, outcome probeOutcome, err error, creds *routertls.Credentials, router, caFile string, attrs ...any) {
	id := creds.SPIFFEID()
	switch outcome {
	case probeAccepted:
		logger.Info("atenet-router accepted the router client", "atenet_router", router, "client_id", id)
	case probeRefused:
		logger.Error("atenet-router refused the router client; add its ID to the router's --ingress-allowed-spiffe-ids", "atenet_router", router, "client_id", id, "error", err)
	case probeUnverified:
		logger.Error("cannot verify the atenet-router's serving certificate", "atenet_router", router, "server_name", creds.ServerName(), "ca_file", caFile, "error", err)
	default:
		logger.Warn("atenet-router probe failed", append([]any{"atenet_router", router, "client_id", id, "error", err}, attrs...)...)
	}
}
