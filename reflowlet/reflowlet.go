// Copyright 2017 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

package reflowlet

import (
	"crypto/tls"
	"flag"
	"io/ioutil"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go/service/s3"
	dockerclient "github.com/docker/engine-api/client"
	"github.com/grailbio/base/limiter"
	"github.com/grailbio/reflow/config"
	"github.com/grailbio/reflow/internal/ec2authenticator"
	"github.com/grailbio/reflow/local"
	"github.com/grailbio/reflow/log"
	"github.com/grailbio/reflow/pool/server"
	repositoryhttp "github.com/grailbio/reflow/repository/http"
	reflows3 "github.com/grailbio/reflow/repository/s3"
	"github.com/grailbio/reflow/rest"
	"golang.org/x/net/http2"
)

// maxConcurrentStreams is the number of concurrent http/2 streams we
// support.
const maxConcurrentStreams = 20000

// A Server is a reflow server, exposing a local pool over an HTTP server.
type Server struct {
	// The server's config.
	// TODO(marius): move most of what is now flags here into the config.
	Config config.Config

	// Addr is the address on which to listen.
	Addr string
	// Prefix is the prefix used for directory lookup; permits reflowlet
	// to run inside of Docker.
	Prefix string
	// Insecure listens on HTTP, not HTTPS.
	Insecure bool
	// Dir is the runtime data directory.
	Dir string
	// NDigest is the number of allowable concurrent digest operations.
	NDigest int
	// EC2Cluster tells whether this reflowlet is part of an EC2cluster.
	// When true, the reflowlet shuts down if it is idle after 10 minutes.
	EC2Cluster bool
	// HTTPDebug determines whether HTTP debug logging is turned on.
	HTTPDebug bool

	configFlag string
}

// AddFlags adds flags configuring various Reflowlet parameters to
// the provided FlagSet.
func (s *Server) AddFlags(flags *flag.FlagSet) {
	flags.StringVar(&s.configFlag, "config", "", "the Reflow configuration file")
	flags.StringVar(&s.Addr, "addr", ":9000", "HTTPS server address")
	flags.StringVar(&s.Prefix, "prefix", "", "prefix used for directory lookup")
	flags.BoolVar(&s.Insecure, "insecure", false, "listen on HTTP, not HTTPS")
	flags.StringVar(&s.Dir, "dir", "/mnt/data/reflow", "runtime data directory")
	flags.IntVar(&s.NDigest, "ndigest", 32, "number of allowable concurrent digest ops")
	flags.BoolVar(&s.EC2Cluster, "ec2cluster", false, "this reflowlet is part of an ec2cluster")
	flags.BoolVar(&s.HTTPDebug, "httpdebug", false, "turn on HTTP debug logging")
}

// ListenAndServe serves the Reflowlet server on the configured address.
func (s *Server) ListenAndServe() error {
	if s.configFlag != "" {
		b, err := ioutil.ReadFile(s.configFlag)
		if err != nil {
			return err
		}
		if err := config.Unmarshal(b, s.Config.Keys()); err != nil {
			return err
		}
	}
	var err error
	s.Config, err = config.Make(s.Config)
	if err != nil {
		return err
	}
	addr := os.Getenv("DOCKER_HOST")
	if addr == "" {
		addr = "unix:///var/run/docker.sock"
	}
	client, err := dockerclient.NewClient(
		addr, dockerclient.DefaultVersion,
		nil, map[string]string{"user-agent": "reflow"})
	if err != nil {
		return err
	}

	sess, err := s.Config.AWS()
	if err != nil {
		return err
	}
	clientConfig, serverConfig, err := s.Config.HTTPS()
	if err != nil {
		return err
	}
	creds, err := s.Config.AWSCreds()
	if err != nil {
		return err
	}
	tool, err := s.Config.AWSTool()
	if err != nil {
		return err
	}

	// Default HTTPS and s3 clients for repository dialers.
	// TODO(marius): handle this more elegantly, perhaps by
	// avoiding global registration altogether.
	reflows3.SetClient(s3.New(sess))
	transport := &http.Transport{TLSClientConfig: clientConfig}
	http2.ConfigureTransport(transport)
	repositoryhttp.HTTPClient = &http.Client{Transport: transport}

	lim := limiter.New()
	lim.Release(s.NDigest)
	p := &local.Pool{
		Client:        client,
		Dir:           s.Dir,
		Prefix:        s.Prefix,
		Authenticator: ec2authenticator.New(sess),
		AWSImage:      tool,
		AWSCreds:      creds,
		Log:           log.Std.Tee(nil, "executor: "),
		DigestLimiter: lim,
	}
	if err := p.Start(); err != nil {
		return err
	}
	if s.EC2Cluster {
		go func() {
			const (
				period = time.Minute
				expiry = 10 * time.Minute
			)
			// Always give the instance an expiry period to receive work,
			// then check periodically if the instance has been idle for more
			// than the expiry time.
			time.Sleep(expiry)
			for {
				if p.StopIfIdleFor(expiry) {
					log.Fatalf("reflowlet idle for %s; shutting down", expiry)
				}
				time.Sleep(period)
			}
		}()
	}

	var httpLog *log.Logger
	if s.HTTPDebug {
		httpLog = log.Std.Tee(nil, "http: ")
		httpLog.Level = log.DebugLevel
		log.Std.Level = log.DebugLevel
	}

	http.Handle("/", rest.Handler(server.NewNode(p), httpLog))
	server := &http.Server{Addr: s.Addr}
	if s.Insecure {
		return server.ListenAndServe()
	}
	serverConfig.ClientAuth = tls.RequireAndVerifyClientCert
	server.TLSConfig = serverConfig
	http2.ConfigureServer(server, &http2.Server{
		MaxConcurrentStreams: maxConcurrentStreams,
	})
	return server.ListenAndServeTLS("", "")
}

// IgnoreSigpipe consumes (and ignores) SIGPIPE signals. As of Go
// 1.6, these are generated only for stdout and stderr.
//
// This is useful where a reflowlet's standard output is closed while
// running, as can happen when journald restarts on systemd managed
// systems.
//
// See the following for more information:
//	https://bugzilla.redhat.com/show_bug.cgi?id=1300076
func IgnoreSigpipe() {
	c := make(chan os.Signal, 1024)
	signal.Notify(c, os.Signal(syscall.SIGPIPE))
	for {
		<-c
	}
}
