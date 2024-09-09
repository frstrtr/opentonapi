package api

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"

	"github.com/tonkeeper/tongo/config"
	"go.uber.org/zap"

	"github.com/tonkeeper/opentonapi/pkg/oas"
	"github.com/tonkeeper/opentonapi/pkg/pusher/sources"
	"github.com/tonkeeper/opentonapi/pkg/pusher/sse"
	"github.com/tonkeeper/opentonapi/pkg/pusher/websocket"
)

// Server opens a port and exposes REST-ish API.
//
// Server integrates two groups of endpoints:
//  1. The first group named "Ogen" consists of endpoints generated by ogen based on api/openapi.yml.
//     It has an independent server in "oas" package.
//  2. The second group named "Async" contains methods that aren't supported by ogen (streaming methods with non-standard Content-Type).
//     These methods are defined manually and are exposed with http.ServeMux.
//
// We provide basic middleware like logging and metrics for both groups.
type Server struct {
	logger           *zap.Logger
	httpServer       *http.Server
	mux              *http.ServeMux
	asyncMiddlewares []AsyncMiddleware
}

// For authentication purposes we need to distinguish between regular and long-lived connections.
const (
	LongLivedConnection = 0
	RegularConnection   = 1
)

type AsyncHandler func(w http.ResponseWriter, r *http.Request, connectionType int, allowTokenInQuery bool) error
type AsyncMiddleware func(AsyncHandler) AsyncHandler

type ServerOptions struct {
	ogenMiddlewares    []oas.Middleware
	asyncMiddlewares   []AsyncMiddleware
	txSource           sources.TransactionSource
	blockHeadersSource sources.BlockHeadersSource
	blockSource        sources.BlockSource
	traceSource        sources.TraceSource
	memPool            sources.MemPoolSource
	liteServers        []config.LiteServer
}

type ServerOption func(options *ServerOptions)

func WithOgenMiddleware(m ...oas.Middleware) ServerOption {
	return func(options *ServerOptions) {
		options.ogenMiddlewares = m
	}
}

func WithAsyncMiddleware(m ...AsyncMiddleware) ServerOption {
	return func(options *ServerOptions) {
		options.asyncMiddlewares = m
	}
}

func WithBlockHeadersSource(src sources.BlockHeadersSource) ServerOption {
	return func(options *ServerOptions) {
		options.blockHeadersSource = src
	}
}

func WithBlockSource(src sources.BlockSource) ServerOption {
	return func(options *ServerOptions) {
		options.blockSource = src
	}
}

func WithTransactionSource(txSource sources.TransactionSource) ServerOption {
	return func(options *ServerOptions) {
		options.txSource = txSource
	}
}

func WithTraceSource(src sources.TraceSource) ServerOption {
	return func(options *ServerOptions) {
		options.traceSource = src
	}
}

func WithMemPool(memPool sources.MemPoolSource) ServerOption {
	return func(options *ServerOptions) {
		options.memPool = memPool
	}
}

func NewServer(log *zap.Logger, handler *Handler, opts ...ServerOption) (*Server, error) {
	options := &ServerOptions{}
	for _, o := range opts {
		o(options)
	}
	ogenMiddlewares := []oas.Middleware{ogenLoggingMiddleware(log), ogenMetricsMiddleware}
	ogenMiddlewares = append(ogenMiddlewares, options.ogenMiddlewares...)

	ogenServer, err := oas.NewServer(handler,
		oas.WithMiddleware(ogenMiddlewares...),
		oas.WithErrorHandler(ogenErrorsHandler))
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	asyncMiddlewares := []AsyncMiddleware{asyncLoggingMiddleware(log), asyncMetricsMiddleware}
	asyncMiddlewares = append(asyncMiddlewares, options.asyncMiddlewares...)

	sseHandler := sse.NewHandler(options.blockSource, options.blockHeadersSource, options.txSource, options.traceSource, options.memPool)
	if options.blockSource != nil {
		mux.Handle("/v2/sse/blockchain/full", wrapAsync(LongLivedConnection, true, chainMiddlewares(sse.Stream(log, sseHandler.SubscribeToBlocks), asyncMiddlewares...)))
	}
	if options.blockHeadersSource != nil {
		mux.Handle("/v2/sse/blocks", wrapAsync(LongLivedConnection, true, chainMiddlewares(sse.Stream(log, sseHandler.SubscribeToBlockHeaders), asyncMiddlewares...)))
	}
	if options.txSource != nil {
		mux.Handle("/v2/sse/accounts/transactions", wrapAsync(LongLivedConnection, true, chainMiddlewares(sse.Stream(log, sseHandler.SubscribeToTransactions), asyncMiddlewares...)))
	}
	if options.traceSource != nil {
		mux.Handle("/v2/sse/accounts/traces", wrapAsync(LongLivedConnection, true, chainMiddlewares(sse.Stream(log, sseHandler.SubscribeToTraces), asyncMiddlewares...)))
	}
	if options.memPool != nil {
		mux.Handle("/v2/sse/mempool", wrapAsync(LongLivedConnection, true, chainMiddlewares(sse.Stream(log, sseHandler.SubscribeToMessages), asyncMiddlewares...)))
	}

	websocketHandler := websocket.Handler(log, options.txSource, options.traceSource, options.memPool, options.blockHeadersSource)
	mux.Handle("/v2/websocket", wrapAsync(LongLivedConnection, true, chainMiddlewares(websocketHandler, asyncMiddlewares...)))
	mux.Handle("/", ogenServer)

	serv := Server{
		logger:           log,
		mux:              mux,
		asyncMiddlewares: asyncMiddlewares,
		httpServer: &http.Server{
			Handler: mux,
		},
	}
	return &serv, nil
}

func wrapAsync(connectionType int, allowTokenInQuery bool, handler AsyncHandler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_ = handler(writer, request, connectionType, allowTokenInQuery)
	})
}

func chainMiddlewares(handler AsyncHandler, middleware ...AsyncMiddleware) AsyncHandler {
	for _, md := range middleware {
		handler = md(handler)
	}
	return handler
}

func (s *Server) RegisterAsyncHandler(pattern string, handler AsyncHandler, connectionType int, allowTokenInQuery bool) {
	s.mux.Handle(pattern, wrapAsync(connectionType, allowTokenInQuery, chainMiddlewares(handler, s.asyncMiddlewares...)))
}

func (s *Server) Run(address string, unixSockets []string) {
	go func() {
		tcpListener, err := net.Listen("tcp", address)
		if err != nil {
			s.logger.Fatal("Failed to listen on tcp address", zap.Error(err))
		}
		err = s.httpServer.Serve(tcpListener)

		if errors.Is(err, http.ErrServerClosed) {
			s.logger.Warn("opentonapi quit")
			return
		}
		s.logger.Fatal("ListenAndServe() failed", zap.Error(err))
	}()

	for _, socketPath := range unixSockets {
		go func(socketPath string) {
			if _, err := os.Stat(socketPath); err == nil {
				os.Remove(socketPath)
			}

			unixListener, err := net.Listen("unix", socketPath)
			if err != nil {
				s.logger.Fatal(fmt.Sprintf("Failed to listen on Unix socket %v", socketPath), zap.Error(err))
			}

			if err := os.Chmod(socketPath, fs.ModePerm); err != nil {
				s.logger.Fatal(fmt.Sprintf("Failed to set permissions on Unix socket %v", socketPath), zap.Error(err))
			}

			err = s.httpServer.Serve(unixListener)
			if errors.Is(err, http.ErrServerClosed) {
				s.logger.Warn("opentonapi quit")
				return
			}
			s.logger.Fatal(fmt.Sprintf("ListenAndServe() failed for %v", socketPath), zap.Error(err))
		}(socketPath)
	}
	<-make(chan struct{})
}
