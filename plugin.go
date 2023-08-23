package crossover

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	//"fmt"
	//"log"
	"net/http"
	"regexp"
	"time"
)

const (
	defaultTimeout       = 5
	defaultAPIKEY        = "c7f1f03dde5fc0cab9aa53081ed08ab797ff54e52e6ff4e9a38e3e092ffcf7c5"
	defaultRemoteAddress = "http://localhost:8083/logs"
	defaultPattern       = "([0-9a-z]{8}-[0-9a-z]{4}-[0-9a-z]{4}-[0-9a-z]{4}-[0-9a-z]{12})"
)

// Config holds configuration to passed to the plugin
type Config struct {
	Pattern       string
	RemoteAddress string
	APIKey        string
}

// CreateConfig populates the config data object
func CreateConfig() *Config {
	return &Config{
		Pattern:       defaultPattern,
		RemoteAddress: defaultRemoteAddress,
		APIKey:        defaultAPIKEY,
	}
}

type RequestLogger struct {
	next          http.Handler
	name          string
	client        http.Client
	pattern       string
	remoteAddress string
	apiKey        string
}

// New created a new  plugin.
func New(ctx context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	if len(config.APIKey) == 0 {
		return nil, fmt.Errorf("APIKey can't be empty")
	}
	if len(config.Pattern) == 0 {
		return nil, fmt.Errorf("Pattern can't be empty")
	}
	if len(config.RemoteAddress) == 0 {
		return nil, fmt.Errorf("RemoteAddress can't be empty")
	}

	return &RequestLogger{
		next: next,
		name: name,
		client: http.Client{
			Timeout: defaultTimeout * time.Second,
		},
		pattern:       config.Pattern,
		remoteAddress: config.RemoteAddress,
		apiKey:        config.APIKey,
	}, nil
}

func (a *RequestLogger) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	a.log(req)
	a.next.ServeHTTP(rw, req)
}

func (a *RequestLogger) log(req *http.Request) error {
	requestId := requestKey(a.pattern, req.URL.Path)

	requestBody := map[string]string{"request_id": requestId}
	jsonBody, err := json.Marshal(requestBody)
	if err != nil {
		return err
	}

	bodyReader := bytes.NewReader(jsonBody)
	httpReq, err := http.NewRequest(http.MethodPost, a.remoteAddress, bodyReader)
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Api-Key", a.apiKey)

	httpRes, err := a.client.Do(httpReq)
	if err != nil {
		return err
	}

	if httpRes.StatusCode != http.StatusOK {
		return err
	}
	return nil
}

func requestKey(pattern string, path string) string {
	// Compile the regular expression
	re := regexp.MustCompile(pattern)
	// Find the first match of the pattern in the URL Path
	match := re.FindStringSubmatch(path)

	if len(match) == 0 {
		return ""
	}
	return match[0]
}
