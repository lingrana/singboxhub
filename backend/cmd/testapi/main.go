package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/sing-hub/panel/internal/clash"
)

func main() {
	apiURL := flag.String("url", os.Getenv("SINGHUB_TEST_API_URL"), "Clash API URL")
	secret := flag.String("secret", os.Getenv("SINGHUB_TEST_API_SECRET"), "Clash API secret")
	flag.Parse()
	if strings.TrimSpace(*apiURL) == "" || strings.TrimSpace(*secret) == "" {
		fmt.Fprintln(os.Stderr, "usage: go run ./cmd/testapi -url http://127.0.0.1:19999 -secret abc")
		fmt.Fprintln(os.Stderr, "or set SINGHUB_TEST_API_URL and SINGHUB_TEST_API_SECRET")
		os.Exit(2)
	}

	for i := 1; i <= 5; i++ {
		c, err := clash.New(*apiURL, *secret)
		if err != nil {
			fmt.Printf("request %d: create client error: %v\n", i, err)
			continue
		}
		v, err := c.Version(context.Background())
		if err != nil {
			fmt.Printf("request %d: ERROR: %v\n", i, err)
			continue
		}
		fmt.Printf("request %d: VERSION=%s LATENCY=%dms\n", i, v.Version, v.LatencyMS)
	}
}
