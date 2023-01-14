package main

import (
	"fmt"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/throttled/throttled/v2"
	"github.com/throttled/throttled/v2/store/memstore"
	"gopkg.in/yaml.v3"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

type GatewayItem struct {
	Frontend     string `yaml:"frontend"`
	Backend      string `yaml:"backend"`
	MaxReqPerSec int    `yaml:"reqsPerSec"`
	MaxBurst     int    `yaml:"burst"`
	Label        string `yaml:"label"`
}

type Configuration struct {
	Routes  []GatewayItem `yaml:"routes"`
	Metrics bool          `yaml:"metrics"`
	Port    string        `yaml:"port"`
}

type ResponseTime struct {
	responseTimeHistogram *prometheus.HistogramVec
}

func (resp *ResponseTime) Collect(method string, route string, code string, responseTime float64) {
	resp.responseTimeHistogram.With(prometheus.Labels{
		"method": method,
		"route":  route,
		"code":   code,
	}).Observe(responseTime)
}

func NewResponseTime(label string) *ResponseTime {
	responseTimeHistogram := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    fmt.Sprintf("%s_http_request_duration_ms", label),
		Help:    fmt.Sprintf("Duration of HTTP requests received by the %s endpoint in ms", label),
		Buckets: []float64{.1, 5, 15, 50, 100, 200, 300, 400, 500, 1000},
	}, []string{"method", "route", "code"})
	prometheus.MustRegister(responseTimeHistogram)
	return &ResponseTime{
		responseTimeHistogram,
	}
}

func RPHandler(backend string, requestTotalCounter prometheus.Counter, responseTimeCollector *ResponseTime) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		if requestTotalCounter != nil {
			requestTotalCounter.Inc()
		}

		req, err := http.NewRequest(r.Method, backend, r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			if responseTimeCollector != nil {
				responseTimeCollector.Collect(r.Method, r.RequestURI, strconv.Itoa(http.StatusInternalServerError), float64(time.Since(start).Milliseconds()))
			}
			return
		}

		for k, v := range r.Header {
			req.Header[k] = v
		}

		client := &http.Client{}

		resp, err := client.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			if responseTimeCollector != nil {
				responseTimeCollector.Collect(r.Method, r.RequestURI, strconv.Itoa(http.StatusInternalServerError), float64(time.Since(start).Milliseconds()))
			}
			return
		}
		defer resp.Body.Close()

		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)

		if _, err := io.Copy(w, resp.Body); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			if responseTimeCollector != nil {
				responseTimeCollector.Collect(r.Method, r.RequestURI, strconv.Itoa(http.StatusInternalServerError), float64(time.Since(start).Milliseconds()))
			}
		}
		if responseTimeCollector != nil {
			responseTimeCollector.Collect(r.Method, r.RequestURI, strconv.Itoa(http.StatusOK), float64(time.Since(start).Milliseconds()))
		}
	}
}

func LoadGateway(mux *http.ServeMux, store *memstore.MemStore, items []GatewayItem, metrics bool) {
	for _, i := range items {
		quota := throttled.RateQuota{MaxRate: throttled.PerSec(i.MaxReqPerSec), MaxBurst: i.MaxBurst}
		rateLimiter, err := throttled.NewGCRARateLimiter(store, quota)
		if err != nil {
			log.Fatal(err)
		}

		httpRateLimiter := throttled.HTTPRateLimiter{
			RateLimiter: rateLimiter,
			VaryBy:      &throttled.VaryBy{Path: true},
		}

		if metrics {
			requestTotalCounter := prometheus.NewCounter(prometheus.CounterOpts{
				Name: fmt.Sprintf("%s_requests_total", i.Label),
				Help: fmt.Sprintf("The total number of requests received by the %s endpoint.", i.Label),
			})
			prometheus.MustRegister(requestTotalCounter)
			responseTimeCollector := NewResponseTime(i.Label)

			mux.Handle(i.Frontend, httpRateLimiter.RateLimit(http.HandlerFunc(RPHandler(i.Backend, requestTotalCounter, responseTimeCollector))))
		} else {
			mux.Handle(i.Frontend, httpRateLimiter.RateLimit(http.HandlerFunc(RPHandler(i.Backend, nil, nil))))
		}
	}
}

func main() {
	var config Configuration

	data, err := os.ReadFile("rockhopper.yaml")
	if err != nil {
		log.Fatal("readfile err", err)
	}

	err = yaml.Unmarshal(data, &config)
	if err != nil {
		log.Fatal("unmarshal err", err)
	}

	mux := http.NewServeMux()

	store, err := memstore.New(65536)
	if err != nil {
		log.Fatal(err)
	}

	LoadGateway(mux, store, config.Routes, config.Metrics)

	if config.Metrics {
		mux.Handle("/metrics", promhttp.Handler())
	}

	srv := &http.Server{
		Handler:      mux,
		Addr:         fmt.Sprintf("127.0.0.1:%s", config.Port),
		WriteTimeout: 15 * time.Second,
		ReadTimeout:  15 * time.Second,
	}

	fmt.Printf("🐧 ice-flow-limiter service is running http://127.0.0.1:%s\n", config.Port)
	fmt.Println("Loaded routes :")
	for _, i := range config.Routes {
		fmt.Printf("http://127.0.0.1:%s%s => %s - ratelimit: %v - burst: %v\n", config.Port, i.Frontend, i.Backend, i.MaxReqPerSec, i.MaxBurst)
	}
	log.Fatal(srv.ListenAndServe())
}
