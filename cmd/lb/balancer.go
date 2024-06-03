package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/DanyloM73/arch-lab-5/httptools"
	"github.com/DanyloM73/arch-lab-5/signal"
)

var (
	port       = flag.Int("port", 8090, "load balancer port")
	timeoutSec = flag.Int("timeout-sec", 3, "request timeout time in seconds")
	https      = flag.Bool("https", false, "whether backends support HTTPs")

	traceEnabled = flag.Bool("trace", false, "whether to include tracing information into responses")
)

var (
	timeout     = time.Duration(*timeoutSec) * time.Second
	serversPool = []string{
		"server1:8080",
		"server2:8080",
		"server3:8080",
	}
	healthyServers = make([]bool, len(serversPool))
	mu             sync.Mutex
)

func scheme() string {
	if *https {
		return "https"
	}
	return "http"
}

func health(dst string) bool {
	log.Println("Checking health of", dst)
	ctx, _ := context.WithTimeout(context.Background(), timeout)
	req, _ := http.NewRequestWithContext(ctx, "GET",
		fmt.Sprintf("%s://%s/health", scheme(), dst), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("Failed to check health of %s: %s", dst, err)
		return false
	}
	if resp.StatusCode != http.StatusOK {
		log.Printf("Failed to check health of %s: status code %d", dst, resp.StatusCode)
		return false
	}

	log.Println("Health check of", dst, "succeeded")
	return true
}

func forward(dst string, rw http.ResponseWriter, r *http.Request) error {
	ctx, _ := context.WithTimeout(r.Context(), timeout)
	fwdRequest := r.Clone(ctx)
	fwdRequest.RequestURI = ""
	fwdRequest.URL.Host = dst
	fwdRequest.URL.Scheme = scheme()
	fwdRequest.Host = dst

	resp, err := http.DefaultClient.Do(fwdRequest)
	if err == nil {
		for k, values := range resp.Header {
			for _, value := range values {
				rw.Header().Add(k, value)
			}
		}
		if *traceEnabled {
			rw.Header().Set("lb-from", dst)
		}
		log.Println("fwd", resp.StatusCode, resp.Request.URL)
		rw.WriteHeader(resp.StatusCode)
		defer resp.Body.Close()
		_, err := io.Copy(rw, resp.Body)
		if err != nil {
			log.Printf("Failed to write response: %s", err)
		}
		return nil
	} else {
		log.Printf("Failed to get response from %s: %s", dst, err)
		rw.WriteHeader(http.StatusServiceUnavailable)
		return err
	}
}

func hash(s string) uint32 {
	h := sha256.New()
	h.Write([]byte(s))
	return binary.BigEndian.Uint32(h.Sum(nil))
}

func updateHealthyServers() {
	for i, server := range serversPool {
		server := server
		i := i
		go func() {
			for range time.Tick(10 * time.Second) {
				mu.Lock()
				healthyServers[i] = health(server)
				mu.Unlock()
			}
		}()
	}
}

func chooseServer(path string) string {
	mu.Lock()
	defer mu.Unlock()

	serverIndex := hash(path) % uint32(len(serversPool))

	originalIndex := serverIndex
	for !healthyServers[serverIndex] {
		serverIndex = (serverIndex + 1) % uint32(len(serversPool))
		if serverIndex == originalIndex {
			return ""
		}
	}

	return serversPool[serverIndex]
}

func main() {
	flag.Parse()

	updateHealthyServers()

	frontend := httptools.CreateServer(*port, http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		server := chooseServer(r.URL.Path)
		if server == "" {
			http.Error(rw, "No healthy servers available", http.StatusServiceUnavailable)
			return
		}

		err := forward(server, rw, r)
		if err != nil {
			log.Printf("Failed to forward request: %s", err)
		}
	}))

	log.Println("Starting load balancer...")
	log.Printf("Tracing support enabled: %t", *traceEnabled)
	frontend.Start()
	signal.WaitForTerminationSignal()
}
