package grpc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/balancer/roundrobin"
	"google.golang.org/grpc/credentials"

	"github.com/pomerium/pomerium/internal/log"
	"github.com/pomerium/pomerium/internal/telemetry"
	"github.com/pomerium/pomerium/internal/telemetry/requestid"
)

const (
	defaultGRPCSecurePort   = 443
	defaultGRPCInsecurePort = 80
)

// Options contains options for connecting to a pomerium rpc service.
type Options struct {
	// Addr is the location of the service.  e.g. "service.corp.example:8443"
	Addr *url.URL
	// OverrideCertificateName overrides the server name used to verify the hostname on the
	// returned certificates from the server. gRPC internals also use it to override the virtual
	// hosting name if it is set.
	OverrideCertificateName string
	// CA specifies the base64 encoded TLS certificate authority to use.
	CA string
	// CAFile specifies the TLS certificate authority file to use.
	CAFile string
	// RequestTimeout specifies the timeout for individual RPC calls
	RequestTimeout time.Duration
	// ClientDNSRoundRobin enables or disables DNS resolver based load balancing
	ClientDNSRoundRobin bool

	// WithInsecure disables transport security for this ClientConn.
	// Note that transport security is required unless WithInsecure is set.
	WithInsecure bool

	// ServiceName specifies the service name for telemetry exposition
	ServiceName string
}

// NewGRPCClientConn returns a new gRPC pomerium service client connection.
func NewGRPCClientConn(opts *Options) (*grpc.ClientConn, error) {
	if opts.Addr == nil {
		return nil, errors.New("internal/grpc: connection address required")
	}
	connAddr := opts.Addr.Host

	// no colon exists in the connection string, assume one must be added manually
	if _, _, err := net.SplitHostPort(connAddr); err != nil {
		if opts.Addr.Scheme == "https" {
			connAddr = net.JoinHostPort(connAddr, strconv.Itoa(defaultGRPCSecurePort))
		} else {
			connAddr = net.JoinHostPort(connAddr, strconv.Itoa(defaultGRPCInsecurePort))
		}
	}

	dialOptions := []grpc.DialOption{
		grpc.WithChainUnaryInterceptor(
			requestid.UnaryClientInterceptor(),
			grpcTimeoutInterceptor(opts.RequestTimeout),
		),
		grpc.WithStreamInterceptor(requestid.StreamClientInterceptor()),
		grpc.WithDefaultCallOptions([]grpc.CallOption{grpc.WaitForReady(true)}...),
	}

	clientStatsHandler := telemetry.NewGRPCClientStatsHandler(opts.ServiceName)
	dialOptions = clientStatsHandler.DialOptions(dialOptions...)

	if opts.WithInsecure {
		log.Info().Str("addr", connAddr).Msg("internal/grpc: grpc with insecure")
		dialOptions = append(dialOptions, grpc.WithInsecure())
	} else {
		rootCAs, err := x509.SystemCertPool()
		if err != nil {
			log.Warn().Msg("internal/grpc: failed getting system cert pool making new one")
			rootCAs = x509.NewCertPool()
		}
		if opts.CA != "" || opts.CAFile != "" {
			var ca []byte
			var err error
			if opts.CA != "" {
				ca, err = base64.StdEncoding.DecodeString(opts.CA)
				if err != nil {
					return nil, fmt.Errorf("failed to decode certificate authority: %w", err)
				}
			} else {
				ca, err = ioutil.ReadFile(opts.CAFile)
				if err != nil {
					return nil, fmt.Errorf("certificate authority file %v not readable: %w", opts.CAFile, err)
				}
			}
			if ok := rootCAs.AppendCertsFromPEM(ca); !ok {
				return nil, fmt.Errorf("failed to append CA cert to certPool")
			}
			log.Debug().Msg("internal/grpc: added custom certificate authority")
		}

		cert := credentials.NewTLS(&tls.Config{RootCAs: rootCAs})

		// override allowed certificate name string, typically used when doing behind ingress connection
		if opts.OverrideCertificateName != "" {
			log.Debug().Str("cert-override-name", opts.OverrideCertificateName).Msg("internal/grpc: grpc")
			err := cert.OverrideServerName(opts.OverrideCertificateName)
			if err != nil {
				return nil, err
			}
		}
		// finally add our credential
		dialOptions = append(dialOptions, grpc.WithTransportCredentials(cert))
	}

	if opts.ClientDNSRoundRobin {
		dialOptions = append(dialOptions, grpc.WithBalancerName(roundrobin.Name), grpc.WithDisableServiceConfig())
		connAddr = fmt.Sprintf("dns:///%s", connAddr)
	}
	return grpc.Dial(
		connAddr,
		dialOptions...,
	)
}

// grpcTimeoutInterceptor enforces per-RPC request timeouts
func grpcTimeoutInterceptor(timeout time.Duration) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if timeout <= 0 {
			return invoker(ctx, method, req, reply, cc, opts...)
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

type grpcClientConnRecord struct {
	conn *grpc.ClientConn
	opts *Options
}

var grpcClientConns = struct {
	sync.Mutex
	m map[string]grpcClientConnRecord
}{
	m: make(map[string]grpcClientConnRecord),
}

// GetGRPCClientConn returns a gRPC client connection for the given name. If a connection for that name has already been
// established the existing connection will be returned. If any options change for that connection, the existing
// connection will be closed and a new one established.
func GetGRPCClientConn(name string, opts *Options) (*grpc.ClientConn, error) {
	grpcClientConns.Lock()
	defer grpcClientConns.Unlock()

	current, ok := grpcClientConns.m[name]
	if ok {
		if cmp.Equal(current.opts, opts) {
			return current.conn, nil
		}

		err := current.conn.Close()
		if err != nil {
			log.Error().Err(err).Msg("grpc: failed to close existing connection")
		}
	}

	cc, err := NewGRPCClientConn(opts)
	if err != nil {
		return nil, err
	}

	grpcClientConns.m[name] = grpcClientConnRecord{
		conn: cc,
		opts: opts,
	}
	return cc, nil
}
