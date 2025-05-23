package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/naviat/solana-rpc-exporter/pkg/slog"
	"go.uber.org/zap"
)

type cachedValue[T any] struct {
	value     T
	timestamp time.Time
}

const (
	// LamportsInSol is the number of lamports in 1 SOL
	LamportsInSol = 1_000_000_000

	CommitmentFinalized Commitment = "finalized"
	CommitmentConfirmed Commitment = "confirmed"
	CommitmentProcessed Commitment = "processed"
)

type (
	Client struct {
		HttpClient  http.Client
		RpcUrl      string
		HttpTimeout time.Duration
		logger      *zap.SugaredLogger

		// Cache fields
		cacheMutex    sync.RWMutex
		versionCache  *cachedValue[string]
		healthCache   *cachedValue[string]
		cacheValidity time.Duration
	}

	Request struct {
		Jsonrpc string `json:"jsonrpc"`
		Id      int    `json:"id"`
		Method  string `json:"method"`
		Params  []any  `json:"params"`
	}

	Commitment string
)

func NewRPCClient(rpcAddr string, httpTimeout time.Duration) *Client {
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
	}

	return &Client{
		HttpClient: http.Client{
			Transport: transport,
			Timeout:   httpTimeout,
		},
		RpcUrl:        rpcAddr,
		HttpTimeout:   httpTimeout,
		logger:        slog.Get(),
		cacheValidity: 60 * time.Second, // Cache version and health for 1 minute
	}
}

func (c *Client) TestConnection(ctx context.Context) error {
	maxRetries := 3
	retryDelay := time.Second * 2

	var lastErr error
	for i := 0; i < maxRetries; i++ {
		_, err := c.GetVersion(ctx)
		if err == nil {
			return nil
		}

		lastErr = err
		if c.logger != nil {
			c.logger.Warnf("Connection attempt %d/%d failed: %v", i+1, maxRetries, err)
		}

		if i < maxRetries-1 { // Don't sleep after the last attempt
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(retryDelay):
				retryDelay *= 2 // Exponential backoff
			}
		}
	}

	return fmt.Errorf("failed to connect after %d attempts: %w", maxRetries, lastErr)
}

func getResponse[T any](
	ctx context.Context,
	client *Client,
	method string,
	params []any,
	rpcResponse *Response[T],
) error {
	request := &Request{
		Jsonrpc: "2.0",
		Id:      1,
		Method:  method,
		Params:  params,
	}

	buffer, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	if client.logger != nil {
		client.logger.Debugf("Making RPC request to %s: %s", client.RpcUrl, string(buffer))
	}

	req, err := http.NewRequestWithContext(ctx, "POST", client.RpcUrl, bytes.NewBuffer(buffer))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := client.HttpClient.Do(req)
	if err != nil {
		if client.logger != nil {
			client.logger.Errorf("RPC request failed: %v", err)
		}
		return fmt.Errorf("%s RPC call failed: %w", method, err)
	}
	defer resp.Body.Close()

	if client.logger != nil {
		duration := time.Since(start)
		client.logger.Debugw("RPC request completed",
			"method", method,
			"duration_ms", duration.Milliseconds(),
		)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("error reading response: %w", err)
	}

	if client.logger != nil {
		client.logger.Debugf("RPC response: %s", string(body))
	}

	if err = json.Unmarshal(body, rpcResponse); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}

	if rpcResponse.Error.Code != 0 {
		rpcResponse.Error.Method = method
		return &rpcResponse.Error
	}

	return nil
}

// Core RPC methods
func (c *Client) GetBlockTime(ctx context.Context, slot int64) (int64, error) {
	var resp Response[int64]
	if err := getResponse(ctx, c, "getBlockTime", []any{slot}, &resp); err != nil {
		return 0, err
	}
	return resp.Result, nil
}

func (c *Client) GetEpochInfo(ctx context.Context, commitment Commitment) (*EpochInfo, error) {
	var resp Response[EpochInfo]
	config := map[string]string{"commitment": string(commitment)}
	if err := getResponse(ctx, c, "getEpochInfo", []any{config}, &resp); err != nil {
		return nil, err
	}
	return &resp.Result, nil
}

func (c *Client) GetVersion(ctx context.Context) (string, error) {
	// Check cache first
	c.cacheMutex.RLock()
	if c.versionCache != nil && time.Since(c.versionCache.timestamp) < c.cacheValidity {
		version := c.versionCache.value
		c.cacheMutex.RUnlock()
		if c.logger != nil {
			c.logger.Debug("Version returned from cache")
		}
		return version, nil
	}
	c.cacheMutex.RUnlock()

	var resp Response[struct {
		Version string `json:"solana-core"`
	}]
	if err := getResponse(ctx, c, "getVersion", []any{}, &resp); err != nil {
		return "", err
	}

	// Update cache
	c.cacheMutex.Lock()
	c.versionCache = &cachedValue[string]{
		value:     resp.Result.Version,
		timestamp: time.Now(),
	}
	c.cacheMutex.Unlock()

	return resp.Result.Version, nil
}

func (c *Client) GetHealth(ctx context.Context) (string, error) {
	// Check cache first
	c.cacheMutex.RLock()
	if c.healthCache != nil && time.Since(c.healthCache.timestamp) < c.cacheValidity {
		health := c.healthCache.value
		c.cacheMutex.RUnlock()
		if c.logger != nil {
			c.logger.Debug("Health status returned from cache")
		}
		return health, nil
	}
	c.cacheMutex.RUnlock()

	var resp Response[string]
	if err := getResponse(ctx, c, "getHealth", []any{}, &resp); err != nil {
		return "", err
	}

	// Update cache
	c.cacheMutex.Lock()
	c.healthCache = &cachedValue[string]{
		value:     resp.Result,
		timestamp: time.Now(),
	}
	c.cacheMutex.Unlock()

	return resp.Result, nil
}

func (c *Client) GetMinimumLedgerSlot(ctx context.Context) (int64, error) {
	var resp Response[int64]
	if err := getResponse(ctx, c, "minimumLedgerSlot", []any{}, &resp); err != nil {
		return 0, err
	}
	return resp.Result, nil
}

func (c *Client) GetFirstAvailableBlock(ctx context.Context) (int64, error) {
	var resp Response[int64]
	if err := getResponse(ctx, c, "getFirstAvailableBlock", []any{}, &resp); err != nil {
		return 0, err
	}
	return resp.Result, nil
}
