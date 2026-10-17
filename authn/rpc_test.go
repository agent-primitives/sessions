package authn

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"testing"
	"time"

	pb "github.com/agent-primitives/sessions/api/genpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"
)

// staticVerifier is deliberately test-only: opaque credentials, no cloud or
// issuer discovery. Adapters, not the interceptor, own credential syntax.
type staticVerifier map[string]VerifiedCaller

func (v staticVerifier) Verify(ctx context.Context) (VerifiedCaller, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get("authorization")
	if len(values) == 1 {
		if c, ok := v[values[0]]; ok {
			return c, nil
		}
	}
	return VerifiedCaller{}, errors.New("credential rejected")
}

func TestEveryGeneratedRPC(t *testing.T) {
	v := staticVerifier{
		"Bearer accepted": {testIdentity, time.Now().Add(time.Hour)},
		"Bearer expired":  {testIdentity, time.Now().Add(-time.Second)},
	}
	conn := testConnection(t, v)
	// Descriptors enumerate methods rather than a maintained list of RPC names.
	// Empty protobuf messages decode as default requests for these proto3 APIs.
	for _, service := range []*grpc.ServiceDesc{&pb.Sessions_ServiceDesc, &pb.HarnessRegistry_ServiceDesc, &pb.Harness_ServiceDesc} {
		for _, tc := range []struct {
			name        string
			credentials []string
			code        codes.Code
		}{
			{"missing", nil, codes.Unauthenticated},
			{"malformed", []string{"Bearer"}, codes.Unauthenticated},
			{"unknown", []string{"Bearer unknown"}, codes.Unauthenticated},
			{"duplicate", []string{"Bearer accepted", "Bearer accepted"}, codes.Unauthenticated},
			{"expired", []string{"Bearer expired"}, codes.Unauthenticated},
			{"verified", []string{"Bearer accepted"}, codes.Unimplemented},
		} {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			md := metadata.MD{"authorization": tc.credentials, "x-forwarded-subject": {"forged"}}
			ctx = metadata.NewOutgoingContext(ctx, md)
			for _, method := range service.Methods {
				t.Run(service.ServiceName+"/"+method.MethodName+"/"+tc.name, func(t *testing.T) {
					err := conn.Invoke(ctx, "/"+service.ServiceName+"/"+method.MethodName, &emptypb.Empty{}, &emptypb.Empty{})
					assertCode(t, err, tc.code)
				})
			}
			for _, method := range service.Streams {
				t.Run(service.ServiceName+"/"+method.StreamName+"/"+tc.name, func(t *testing.T) {
					stream, err := conn.NewStream(ctx, &method, "/"+service.ServiceName+"/"+method.StreamName)
					if err != nil {
						t.Fatal(err)
					}
					// Rejection may arrive before the request is sent; RecvMsg owns the status.
					_ = stream.SendMsg(&emptypb.Empty{})
					_ = stream.CloseSend()
					assertCode(t, stream.RecvMsg(&emptypb.Empty{}), tc.code)
				})
			}
			cancel()
		}
	}
}

func TestStreamExpiryOverTLS(t *testing.T) {
	conn := testConnection(t, verifierFunc(func(context.Context) (VerifiedCaller, error) {
		return VerifiedCaller{testIdentity, time.Now().Add(50 * time.Millisecond)}, nil
	}), func(_ any, ss grpc.ServerStream) error {
		<-ss.Context().Done()
		return ss.Context().Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := conn.NewStream(ctx, &pb.Sessions_ServiceDesc.Streams[1], pb.Sessions_Replay_FullMethodName)
	if err != nil {
		t.Fatal(err)
	}
	assertCode(t, stream.RecvMsg(&emptypb.Empty{}), codes.Unauthenticated)
}

func testConnection(t *testing.T, verifier CallerVerifier, streamHandler ...grpc.StreamHandler) *grpc.ClientConn {
	t.Helper()
	u, s, err := NewInterceptors(Required, verifier)
	if err != nil {
		t.Fatal(err)
	}
	serverTLS, clientTLS := testTLS(t)
	check := func(ctx context.Context) {
		t.Helper()
		if c, ok := Caller(ctx); !ok || c != testIdentity {
			t.Error("handler did not receive exact verified caller")
		}
		p, ok := peer.FromContext(ctx)
		if !ok {
			t.Error("handler has no TLS peer")
			return
		}
		if _, ok := p.AuthInfo.(credentials.TLSInfo); !ok {
			t.Error("handler peer is not TLS")
		}
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)),
		grpc.ChainUnaryInterceptor(u, func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
			check(ctx)
			return next(ctx, req)
		}),
		grpc.ChainStreamInterceptor(s, func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, next grpc.StreamHandler) error {
			check(ss.Context())
			return next(srv, ss)
		}))
	desc := pb.Sessions_ServiceDesc
	if len(streamHandler) != 0 {
		desc.Streams = append([]grpc.StreamDesc(nil), desc.Streams...)
		desc.Streams[1].Handler = streamHandler[0]
	}
	server.RegisterService(&desc, &pb.UnimplementedSessionsServer{})
	pb.RegisterHarnessRegistryServer(server, &pb.UnimplementedHarnessRegistryServer{})
	pb.RegisterHarnessServer(server, &pb.UnimplementedHarnessServer{})
	listener := bufconn.Listen(1024 * 1024)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); _ = listener.Close() })
	conn, err := grpc.NewClient("passthrough:///test", grpc.WithTransportCredentials(credentials.NewTLS(clientTLS)),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func testTLS(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{"test.example"},
		NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}},
		&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: "test.example"}
}
