package feeder

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NethermindEth/juno/core/felt"
	"github.com/NethermindEth/juno/utils"
)

type Backoff func(wait time.Duration) time.Duration

type Client struct {
	url        string
	client     *http.Client
	backoff    Backoff
	maxRetries int
	maxWait    time.Duration
	minWait    time.Duration
	log        utils.SimpleLogger
}

func (c *Client) WithBackoff(b Backoff) *Client {
	c.backoff = b
	return c
}

func (c *Client) WithMaxRetries(num int) *Client {
	c.maxRetries = num
	return c
}

func (c *Client) WithMaxWait(d time.Duration) *Client {
	c.maxWait = d
	return c
}

func (c *Client) WithMinWait(d time.Duration) *Client {
	c.minWait = d
	return c
}

func (c *Client) WithLogger(log utils.SimpleLogger) *Client {
	c.log = log
	return c
}

func ExponentialBackoff(wait time.Duration) time.Duration {
	return wait * 2
}

func NopBackoff(d time.Duration) time.Duration {
	return 0
}

type closeTestClient func()

// NewTestClient returns a client and a function to close a test server.
func NewTestClient(network utils.Network) (*Client, closeTestClient) {
	srv := newTestServer(network)
	c := NewClient(srv.URL).WithBackoff(NopBackoff).WithMaxRetries(0)
	c.client = &http.Client{
		Transport: &http.Transport{
			// On macOS tests often fail with the following error:
			//
			// "Get "http://127.0.0.1:xxxx/get_{feeder gateway method}?{arg}={value}": dial tcp 127.0.0.1:xxxx:
			//    connect: can't assign requested address"
			//
			// This error makes running local tests, in quick succession, difficult because we have to wait for the OS to release ports.
			// Sometimes the sync tests will hang because sync process will keep making requests if there was some error.
			// This problem is further exacerbated by having parallel tests.
			//
			// Increasing test client's idle conns allows for large concurrent requests to be made from a single test client.
			MaxIdleConnsPerHost: 1000,
		},
	}
	return c, srv.Close
}

func newTestServer(network utils.Network) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queryMap, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		wd, err := os.Getwd()
		if err != nil {
			panic(err)
		}

		base := wd[:strings.LastIndex(wd, "juno")+4]
		queryArg := ""
		dir := ""

		switch {
		case strings.HasSuffix(r.URL.Path, "get_block"):
			dir = "block"
			queryArg = "blockNumber"
		case strings.HasSuffix(r.URL.Path, "get_state_update"):
			dir = "state_update"
			queryArg = "blockNumber"
		case strings.HasSuffix(r.URL.Path, "get_transaction"):
			dir = "transaction"
			queryArg = "transactionHash"
		case strings.HasSuffix(r.URL.Path, "get_class_by_hash"):
			dir = "class"
			queryArg = "classHash"
		case strings.HasSuffix(r.URL.Path, "get_compiled_class_by_class_hash"):
			dir = "compiled_class"
			queryArg = "classHash"
		}

		fileName, found := queryMap[queryArg]
		if !found {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		path := filepath.Join(base, "clients", "feeder", "testdata", network.String(), dir, fileName[0]+".json")
		read, err := os.ReadFile(path)
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		_, err = w.Write(read)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
}

func NewClient(clientURL string) *Client {
	return &Client{
		url:        clientURL,
		client:     http.DefaultClient,
		backoff:    ExponentialBackoff,
		maxRetries: 35, // ~3.5 minutes with default backoff and maxWait (block time on mainnet is 1-2 minutes)
		maxWait:    10 * time.Second,
		minWait:    time.Second,
		log:        utils.NewNopZapLogger(),
	}
}

// buildQueryString builds the query url with encoded parameters
func (c *Client) buildQueryString(endpoint string, args map[string]string) string {
	base, err := url.Parse(c.url)
	if err != nil {
		panic("Malformed feeder base URL")
	}

	base.Path += endpoint

	params := url.Values{}
	for k, v := range args {
		params.Add(k, v)
	}
	base.RawQuery = params.Encode()

	return base.String()
}

// get performs a "GET" http request with the given URL and returns the response body
func (c *Client) get(ctx context.Context, queryURL string) (io.ReadCloser, error) {
	var res *http.Response
	var err error
	wait := time.Duration(0)
	for i := 0; i <= c.maxRetries; i++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
			var req *http.Request
			req, err = http.NewRequestWithContext(ctx, "GET", queryURL, http.NoBody)
			if err != nil {
				return nil, err
			}

			res, err = c.client.Do(req)
			if err == nil {
				if res.StatusCode == http.StatusOK {
					return res.Body, nil
				} else {
					err = errors.New(res.Status)
				}

				res.Body.Close()
			}

			if wait < c.minWait {
				wait = c.minWait
			}
			wait = c.backoff(wait)
			if wait > c.maxWait {
				wait = c.maxWait
			}
			c.log.Warnw("failed query to feeder, retrying...", "retryAfter", wait.String())
		}
	}
	return nil, err
}

func (c *Client) StateUpdate(ctx context.Context, blockID string) (*StateUpdate, error) {
	queryURL := c.buildQueryString("get_state_update", map[string]string{
		"blockNumber": blockID,
	})

	body, err := c.get(ctx, queryURL)
	if err != nil {
		return nil, err
	}
	defer body.Close()

	update := new(StateUpdate)
	if err = json.NewDecoder(body).Decode(update); err != nil {
		return nil, err
	}
	return update, nil
}

func (c *Client) Transaction(ctx context.Context, transactionHash *felt.Felt) (*TransactionStatus, error) {
	queryURL := c.buildQueryString("get_transaction", map[string]string{
		"transactionHash": transactionHash.String(),
	})

	body, err := c.get(ctx, queryURL)
	if err != nil {
		return nil, err
	}
	defer body.Close()

	txStatus := new(TransactionStatus)
	if err = json.NewDecoder(body).Decode(txStatus); err != nil {
		return nil, err
	}
	return txStatus, nil
}

func (c *Client) Block(ctx context.Context, blockID string) (*Block, error) {
	queryURL := c.buildQueryString("get_block", map[string]string{
		"blockNumber": blockID,
	})

	body, err := c.get(ctx, queryURL)
	if err != nil {
		return nil, err
	}
	defer body.Close()

	block := new(Block)
	if err = json.NewDecoder(body).Decode(block); err != nil {
		return nil, err
	}
	return block, nil
}

func (c *Client) ClassDefinition(ctx context.Context, classHash *felt.Felt) (*ClassDefinition, error) {
	queryURL := c.buildQueryString("get_class_by_hash", map[string]string{
		"classHash": classHash.String(),
	})

	body, err := c.get(ctx, queryURL)
	if err != nil {
		return nil, err
	}
	defer body.Close()

	class := new(ClassDefinition)
	if err = json.NewDecoder(body).Decode(class); err != nil {
		return nil, err
	}
	return class, nil
}

func (c *Client) CompiledClassDefinition(ctx context.Context, classHash *felt.Felt) (json.RawMessage, error) {
	queryURL := c.buildQueryString("get_compiled_class_by_class_hash", map[string]string{
		"classHash": classHash.String(),
	})

	body, err := c.get(ctx, queryURL)
	if err != nil {
		return nil, err
	}
	defer body.Close()

	var class json.RawMessage
	if err = json.NewDecoder(body).Decode(&class); err != nil {
		return nil, err
	}
	return class, nil
}
