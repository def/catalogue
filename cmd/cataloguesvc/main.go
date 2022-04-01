package main

import (
	"flag"
	"fmt"
	"github.com/gorilla/mux"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	stdopentracing "github.com/opentracing/opentracing-go"
	"net/http"

	"github.com/def/catalogue"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/net/context"
	"k8s.io/klog"
	"path/filepath"
)

func main() {
	var (
		port   = flag.String("port", "80", "Port to bind HTTP listener") // TODO(pb): should be -addr, default ":80"
		images = flag.String("images", "./images/", "Image path")
		dsn    = flag.String("DSN", "host=catalogue-db port=5432 user=postgres password=fake_password dbname=socksdb sslmode=disable", "")
	)
	flag.Parse()

	klog.Infof("images: %q\n", *images)
	abs, err := filepath.Abs(*images)
	klog.Infof("Abs(images): %q (%v)\n", abs, err)
	pwd, err := os.Getwd()
	klog.Infof("Getwd: %q (%v)\n", pwd, err)
	files, _ := filepath.Glob(*images + "/*")
	klog.Infof("ls: %q\n", files) // contains a list of all files in the current directory

	// Mechanical stuff.
	errc := make(chan error)
	ctx := context.Background()

	tracer := stdopentracing.NoopTracer{}

	// Data domain.
	db, err := sqlx.Open("postgres", *dsn)
	if err != nil {
		klog.Exitln(err)
		os.Exit(1)
	}
	defer db.Close()

	// Check if DB connection can be made, only for logging purposes, should not fail/exit
	err = db.Ping()
	if err != nil {
		klog.Errorln(err)
	}

	// Service domain.
	var service catalogue.Service
	{
		service = catalogue.NewCatalogueService(db)
		service = catalogue.LoggingMiddleware()(service)
	}

	// Endpoint domain.
	endpoints := catalogue.MakeEndpoints(service, tracer)

	// HTTP router
	router := catalogue.MakeHTTPHandler(ctx, endpoints, *images)

	handler := newRequestMiddleware(router)

	// Create and launch the HTTP server.
	go func() {
		klog.Infoln("transport", "HTTP", "port", *port)
		errc <- http.ListenAndServe(":"+*port, handler)
	}()

	// Capture interrupts.
	go func() {
		c := make(chan os.Signal)
		signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
		errc <- fmt.Errorf("%s", <-c)
	}()

	klog.Infoln("exit", <-errc)
}

type requestMiddleware struct {
	router    *mux.Router
	histogram *prometheus.HistogramVec
}

func newRequestMiddleware(r *mux.Router) http.Handler {
	m := &requestMiddleware{
		router: r,
		histogram: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "Time (in seconds) spent serving HTTP requests.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "path", "status_code"}),
	}
	prometheus.MustRegister(m.histogram)
	return m
}

func (m *requestMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.RequestURI == "/healthz" {
		m.router.ServeHTTP(w, r)
		return
	}

	t := time.Now()
	rw := &responseWriter{w: w, status: http.StatusOK}

	m.router.ServeHTTP(rw, r)

	latency := time.Since(t)

	handler := ""
	var match mux.RouteMatch
	routeExists := m.router.Match(r, &match)
	if routeExists {
		handler, _ = match.Route.GetPathTemplate()
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	m.histogram.WithLabelValues(r.Method, handler, strconv.Itoa(rw.status)).Observe(latency.Seconds())

	f := klog.Infof
	switch {
	case rw.status >= http.StatusInternalServerError:
		f = klog.Errorf
	case rw.status >= http.StatusBadRequest:
		f = klog.Warningf
	}
	f(`%s %s %s %s %d %d %dms`, host, r.Method, r.RequestURI, r.Proto, rw.status, rw.size, latency.Milliseconds())
}

type responseWriter struct {
	w      http.ResponseWriter
	status int
	size   int
}

func (w *responseWriter) Header() http.Header {
	return w.w.Header()
}

func (w *responseWriter) Write(b []byte) (int, error) {
	n, err := w.w.Write(b)
	w.size += n
	return n, err
}

func (w *responseWriter) WriteHeader(statusCode int) {
	w.w.WriteHeader(statusCode)
	w.status = statusCode
}
