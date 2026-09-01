package main

import (
	"net/http"
	"testing"
)

var (
	benchmarkAPIKey      string
	benchmarkAPIKeyError string
)

func BenchmarkExtractAPIKey(b *testing.B) {
	benchmarks := []struct {
		name    string
		headers http.Header
	}{
		{
			name: "authorization only",
			headers: http.Header{
				"Authorization": {"Bearer client-key"},
			},
		},
		{
			name: "typical request",
			headers: http.Header{
				"Accept":          {"application/json"},
				"Accept-Encoding": {"gzip"},
				"Authorization":   {"Bearer client-key"},
				"Content-Length":  {"128"},
				"Content-Type":    {"application/json"},
				"Originator":      {"codex_cli_rs"},
				"User-Agent":      {"codex-cli"},
				"X-Request-Id":    {"request-id"},
			},
		},
		{
			name: "all supported headers",
			headers: http.Header{
				"Authorization":  {"Bearer client-key"},
				"X-Api-Key":      {"client-key"},
				"X-Goog-Api-Key": {"client-key"},
			},
		},
	}
	for _, benchmark := range benchmarks {
		b.Run(benchmark.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				benchmarkAPIKey, benchmarkAPIKeyError = extractAPIKey(benchmark.headers)
			}
		})
	}
}
