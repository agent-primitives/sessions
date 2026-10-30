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

	"github.com/agent-primitives/sessions/integrations/substrate/internal/routertls"
)

// routerProbeTimeout bounds the startup router probe.
var routerProbeTimeout = 10 * time.Second

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

// startRouterProbe probes the router once in the background and logs the outcome, so an operator
// sees an allowlist or trust mistake at startup rather than on the first session. It never gates
// startup or readiness. stop cancels the probe and waits for it.
func (f *substrateFlags) startRouterProbe(logger *slog.Logger) (stop func()) {
	if logger == nil {
		return func() {}
	}
	ctx, cancel := context.WithTimeout(context.Background(), routerProbeTimeout)
	done := make(chan struct{})
	go func() {
		defer close(done)
		outcome, err := probeRouter(ctx, f.routerCreds, *f.router)
		logRouterProbe(logger, outcome, err, f.routerCreds, *f.router, *f.routerCA)
	}()
	return func() {
		cancel()
		<-done
	}
}

func logRouterProbe(logger *slog.Logger, outcome probeOutcome, err error, creds *routertls.Credentials, router, caFile string) {
	id := creds.SPIFFEID()
	switch outcome {
	case probeAccepted:
		logger.Info("atenet-router accepted the router client", "atenet_router", router, "client_id", id)
	case probeRefused:
		logger.Error("atenet-router refused the router client; add its ID to the router's --ingress-allowed-spiffe-ids", "atenet_router", router, "client_id", id, "error", err)
	case probeUnverified:
		logger.Error("cannot verify the atenet-router's serving certificate", "atenet_router", router, "server_name", creds.ServerName(), "ca_file", caFile, "error", err)
	default:
		logger.Warn("atenet-router probe failed", "atenet_router", router, "client_id", id, "error", err)
	}
}
