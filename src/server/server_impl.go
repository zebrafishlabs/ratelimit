package server

import (
	"expvar"
	"fmt"
	"io"
	"net/http"
	"net/http/pprof"
	"sort"
	"strings"

	"os"
	"os/signal"
	"syscall"

	"net"

	"github.com/gorilla/mux"
	reuseport "github.com/kavu/go_reuseport"
	"github.com/lyft/goruntime/loader"
	stats "github.com/lyft/gostats"
	"github.com/lyft/ratelimit/src/settings"
	logger "github.com/sirupsen/logrus"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

type serverDebugListener struct {
	endpoints map[string]string
	debugMux  *http.ServeMux
	listener  net.Listener
}

type server struct {
	port          int
	grpcPort      int
	debugPort     int
	router        *mux.Router
	grpcMux       http.Handler
	grpcServer    *grpc.Server
	store         stats.Store
	scope         stats.Scope
	runtime       loader.IFace
	debugListener serverDebugListener
	health        *HealthChecker
	portTls       bool
}

func (server *server) AddDebugHttpEndpoint(path string, help string, handler http.HandlerFunc) {
	server.debugListener.debugMux.HandleFunc(path, handler)
	server.debugListener.endpoints[path] = help
}

func (server *server) GrpcServer() *grpc.Server {
	return server.grpcServer
}

func (server *server) Start() {
	go func() {
		addr := fmt.Sprintf(":%d", server.debugPort)
		logger.Warnf("Listening for debug on '%s'", addr)
		var err error
		server.debugListener.listener, err = reuseport.Listen("tcp", addr)

		if err != nil {
			logger.Errorf("Failed to open debug HTTP listener: '%+v'", err)
			return
		}
		err = http.Serve(server.debugListener.listener, server.debugListener.debugMux)
		logger.Infof("Failed to start debug server '%+v'", err)
	}()

	go server.startGrpc()

	server.handleGracefulShutdown()

	proto := "HTTP"
	if server.portTls == true {
		proto = "HTTPS"
	}

	addr := fmt.Sprintf(":%d", server.port)
	logger.Warnf("Listening for %s on '%s'", proto, addr)
	list, err := reuseport.Listen("tcp", addr)
	if err != nil {
		logger.Fatalf("Failed to open %s listener: '%+v'", proto, err)
	}

	if server.portTls == true {
		logger.Fatal(http.ServeTLS(list, server.grpcMux, "server_crt.pem", "server_key.pem"))
	} else {
		logger.Fatal(http.Serve(list, server.grpcMux))
	}
}

func (server *server) startGrpc() {
	addr := fmt.Sprintf(":%d", server.grpcPort)
	logger.Warnf("Listening for gRPC on '%s'", addr)
	lis, err := reuseport.Listen("tcp", addr)
	if err != nil {
		logger.Fatalf("Failed to listen for gRPC: %v", err)
	}
	server.grpcServer.Serve(lis)
}

func (server *server) Scope() stats.Scope {
	return server.scope
}

func (server *server) Runtime() loader.IFace {
	return server.runtime
}

type grpcMuxHandler struct {
	grpcServer    *grpc.Server
	health        *HealthChecker
}

func (h grpcMuxHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	logger.Debugf("ServeHTTP Path: %s", r.URL.Path)
	if r.URL.Path == "/" {
//		logger.Infof("/ (ingress healthcheck) received")
    	w.Write([]byte("OK\n"))
//		w.WriteHeader(200)
		return
	}

	if r.URL.Path == "/healthcheck" {
//		logger.Infof("/healthcheck received")
		h.health.ServeHTTP(w, r)
		return
	}

	if isGrpcRequest(r) {
//		logger.Infof("ServeHTTP Path: %s is grpc request.", r.URL.Path)
		h.grpcServer.ServeHTTP(w,r)
		return
	}

//	logger.Infof("ServeHTTP Path: %s is unhandled!", r.URL.Path)
	w.WriteHeader(404)
}

func isGrpcRequest(r *http.Request) bool {
	return r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc")
}

func NewServer(name string, opts ...settings.Option) Server {
	return newServer(name, opts...)
}

func newServer(name string, opts ...settings.Option) *server {
	s := settings.NewSettings()

	for _, opt := range opts {
		opt(&s)
	}

	ret := new(server)
	ret.grpcServer = grpc.NewServer(s.GrpcUnaryInterceptor)

	// setup ports
	ret.port = s.Port
	ret.grpcPort = s.GrpcPort
	ret.debugPort = s.DebugPort
	ret.portTls = s.PortTls

	// setup stats
	ret.store = stats.NewDefaultStore()
	ret.scope = ret.store.Scope(name)
	ret.store.AddStatGenerator(stats.NewRuntimeStats(ret.scope.Scope("go")))

	// setup runtime
	loaderOpts := make([]loader.Option, 0, 1)
	if s.RuntimeIgnoreDotFiles {
		loaderOpts = append(loaderOpts, loader.IgnoreDotFiles)
	} else {
		loaderOpts = append(loaderOpts, loader.AllowDotFiles)
	}

	ret.runtime = loader.New(
		s.RuntimePath,
		s.RuntimeSubdirectory,
		ret.store.Scope("runtime"),
		&loader.SymlinkRefresher{RuntimePath: s.RuntimePath},
		loaderOpts...)

	// setup http router
	ret.router = mux.NewRouter()

	// setup healthcheck path
	ret.health = NewHealthChecker(health.NewServer(), "ratelimit")
//	ret.router.Path("/healthcheck").Handler(ret.health)
	healthpb.RegisterHealthServer(ret.grpcServer, ret.health.Server())

	// setup grpc mux path ... this route must come last so that
	// it is only the default when nothing else matches!
//	ret.grpc_mux = NewGrpcMux()
	health_home := grpcMuxHandler{grpcServer: ret.grpcServer, health: ret.health}
//	ret.router.PathPrefix("/").Handler(health_home)

	ret.grpcMux = h2c.NewHandler(health_home, &http2.Server{})

	// setup default debug listener
	ret.debugListener.debugMux = http.NewServeMux()
	ret.debugListener.endpoints = map[string]string{}
	ret.AddDebugHttpEndpoint(
		"/debug/pprof/",
		"root of various pprof endpoints. hit for help.",
		func(writer http.ResponseWriter, request *http.Request) {
			pprof.Index(writer, request)
		})

	// setup stats endpoint
	ret.AddDebugHttpEndpoint(
		"/stats",
		"print out stats",
		func(writer http.ResponseWriter, request *http.Request) {
			expvar.Do(func(kv expvar.KeyValue) {
				io.WriteString(writer, fmt.Sprintf("%s: %s\n", kv.Key, kv.Value))
			})
		})

	// setup debug root
	ret.debugListener.debugMux.HandleFunc(
		"/",
		func(writer http.ResponseWriter, request *http.Request) {
			sortedKeys := []string{}
			for key := range ret.debugListener.endpoints {
				sortedKeys = append(sortedKeys, key)
			}

			sort.Strings(sortedKeys)
			for _, key := range sortedKeys {
				io.WriteString(
					writer, fmt.Sprintf("%s: %s\n", key, ret.debugListener.endpoints[key]))
			}
		})

	return ret
}

func (server *server) handleGracefulShutdown() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	go func() {
		sig := <-sigs

		logger.Infof("Ratelimit server received %v, shutting down gracefully", sig)
		server.grpcServer.GracefulStop()
		if server.debugListener.listener != nil {
			server.debugListener.listener.Close()
		}
		os.Exit(0)
	}()
}
