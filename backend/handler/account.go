package handler

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type RemoteAccountInfo struct {
	Email            string           `json:"email"`
	UserID           string           `json:"user_id"`
	AccountType      string           `json:"type"`
	Quota            int              `json:"quota"`
	LimitsProgress   []map[string]any `json:"limits_progress"`
	DefaultModelSlug string           `json:"default_model_slug"`
	RestoreAt        string           `json:"restore_at"`
	Status           string           `json:"status"`
}

var accountTypeMap = map[string]string{
	"free":       "Free",
	"plus":       "Plus",
	"personal":   "Plus",
	"pro":        "Pro",
	"team":       "Team",
	"business":   "Team",
	"enterprise": "Team",
}

const proFallbackImageGenQuota = 999

func FetchAccountInfo(ctx context.Context, accessToken string, authData map[string]any, timeout time.Duration) (*RemoteAccountInfo, error) {
	return FetchAccountInfoWithProxy(ctx, accessToken, authData, timeout, "")
}

func FetchAccountInfoWithProxy(ctx context.Context, accessToken string, authData map[string]any, timeout time.Duration, proxyURL string) (*RemoteAccountInfo, error) {
	if strings.TrimSpace(accessToken) == "" {
		return nil, fmt.Errorf("access token is required")
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	client := &http.Client{
		Timeout:   timeout,
		Transport: newChromeTransport(proxyURL),
	}

	meHeaders := buildAccountHeaders(accessToken, authData)
	meHeaders["x-openai-target-path"] = "/backend-api/me"
	meHeaders["x-openai-target-route"] = "/backend-api/me"

	initHeaders := buildAccountHeaders(accessToken, authData)
	initBody := map[string]any{
		"gizmo_id":                nil,
		"requested_default_model": nil,
		"conversation_id":         nil,
		"timezone_offset_min":     -480,
	}

	type responseResult struct {
		payload map[string]any
		err     error
	}

	meCh := make(chan responseResult, 1)
	initCh := make(chan responseResult, 1)

	go func() {
		payload, err := doJSONRequest(ctx, client, http.MethodGet, baseURL+"/me", meHeaders, nil)
		if err != nil {
			meCh <- responseResult{err: fmt.Errorf("/backend-api/me failed: %w", err)}
			return
		}
		meCh <- responseResult{payload: payload}
	}()

	go func() {
		payload, err := doJSONRequest(ctx, client, http.MethodPost, baseURL+"/conversation/init", initHeaders, initBody)
		if err != nil {
			initCh <- responseResult{err: fmt.Errorf("/backend-api/conversation/init failed: %w", err)}
			return
		}
		initCh <- responseResult{payload: payload}
	}()

	meResp := <-meCh
	initResp := <-initCh

	if meResp.err != nil {
		return nil, meResp.err
	}
	if initResp.err != nil {
		return nil, initResp.err
	}

	limitsProgress := normalizeLimitsProgress(initResp.payload["limits_progress"])
	quota, restoreAt := extractQuotaAndRestoreAt(limitsProgress)
	accountType := detectAccountType(accessToken, meResp.payload, initResp.payload)
	if accountType == "Pro" && !hasLimitFeature(limitsProgress, "image_gen") {
		quota = proFallbackImageGenQuota
		limitsProgress = append(limitsProgress, map[string]any{
			"feature_name": "image_gen",
			"remaining":    quota,
		})
	}
	status := "正常"
	if quota == 0 {
		status = "限流"
	}

	return &RemoteAccountInfo{
		Email:            stringValue(meResp.payload["email"]),
		UserID:           stringValue(meResp.payload["id"]),
		AccountType:      accountType,
		Quota:            quota,
		LimitsProgress:   limitsProgress,
		DefaultModelSlug: stringValue(initResp.payload["default_model_slug"]),
		RestoreAt:        restoreAt,
		Status:           status,
	}, nil
}

func buildAccountHeaders(accessToken string, authData map[string]any) map[string]string {
	headers := map[string]string{
		"accept":             "*/*",
		"accept-language":    "zh-CN,zh;q=0.9,en;q=0.8",
		"authorization":      "Bearer " + strings.TrimSpace(accessToken),
		"content-type":       "application/json",
		"oai-language":       "zh-CN",
		"origin":             "https://chatgpt.com",
		"referer":            "https://chatgpt.com/",
		"sec-fetch-dest":     "empty",
		"sec-fetch-mode":     "cors",
		"sec-fetch-site":     "same-origin",
		"user-agent":         stringOrDefault(authData, "user-agent", defaultUserAgent),
		"sec-ch-ua":          stringOrDefault(authData, "sec-ch-ua", `"Google Chrome";v="147", "Not.A/Brand";v="8", "Chromium";v="147"`),
		"sec-ch-ua-mobile":   stringOrDefault(authData, "sec-ch-ua-mobile", "?0"),
		"sec-ch-ua-platform": stringOrDefault(authData, "sec-ch-ua-platform", `"Windows"`),
	}
	if deviceID := firstString(authData, "oai-device-id", "oai_device_id", "device_id"); deviceID != "" {
		headers["oai-device-id"] = deviceID
	}
	if sessionID := firstString(authData, "oai-session-id", "oai_session_id", "session_id"); sessionID != "" {
		headers["oai-session-id"] = sessionID
	}
	if cookies := firstString(authData, "cookies", "cookie"); cookies != "" {
		headers["cookie"] = cookies
	}
	return headers
}

func doJSONRequest(ctx context.Context, client *http.Client, method, url string, headers map[string]string, body any) (map[string]any, error) {
	const maxRetries = 3
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(attempt) * 2 * time.Second
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}
		result, err := doJSONRequestOnce(ctx, client, method, url, headers, body)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if !isRetryableError(err) {
			return nil, err
		}
	}
	return nil, lastErr
}

func doJSONRequestOnce(ctx context.Context, client *http.Client, method, url string, headers map[string]string, body any) (map[string]any, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	for key, value := range headers {
		if strings.TrimSpace(value) != "" {
			req.Header.Set(key, value)
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &nonRetryableError{msg: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}

	payload := map[string]any{}
	if len(data) == 0 {
		return payload, nil
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func detectAccountType(accessToken string, mePayload, initPayload map[string]any) string {
	tokenPayload := decodeAccessTokenPayload(accessToken)

	if authPayload, ok := tokenPayload["https://api.openai.com/auth"].(map[string]any); ok {
		if matched := normalizeAccountType(authPayload["chatgpt_plan_type"]); matched != "" {
			return matched
		}
	}

	for _, payload := range []any{mePayload, initPayload, tokenPayload} {
		if matched := searchAccountType(payload); matched != "" {
			return matched
		}
	}

	return "Free"
}

func decodeAccessTokenPayload(accessToken string) map[string]any {
	parts := strings.Split(strings.TrimSpace(accessToken), ".")
	if len(parts) < 2 {
		return map[string]any{}
	}
	payload := parts[1]
	payload += strings.Repeat("=", (4-len(payload)%4)%4)
	decoded, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return map[string]any{}
	}
	result := map[string]any{}
	if err := json.Unmarshal(decoded, &result); err != nil {
		return map[string]any{}
	}
	return result
}

func normalizeAccountType(value any) string {
	return accountTypeMap[strings.ToLower(strings.TrimSpace(stringValue(value)))]
}

func searchAccountType(value any) string {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			lowerKey := strings.ToLower(strings.TrimSpace(key))
			if matched := normalizeAccountType(item); matched != "" {
				if strings.Contains(lowerKey, "plan") || strings.Contains(lowerKey, "type") || strings.Contains(lowerKey, "subscription") || strings.Contains(lowerKey, "workspace") || strings.Contains(lowerKey, "tier") {
					return matched
				}
			}
		}
		for _, item := range typed {
			if matched := searchAccountType(item); matched != "" {
				return matched
			}
		}
	case []any:
		for _, item := range typed {
			if matched := searchAccountType(item); matched != "" {
				return matched
			}
		}
	default:
		return normalizeAccountType(typed)
	}
	return ""
}

func normalizeLimitsProgress(value any) []map[string]any {
	items, ok := value.([]any)
	if !ok {
		if typed, ok := value.([]map[string]any); ok {
			return typed
		}
		return []map[string]any{}
	}

	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if typed, ok := item.(map[string]any); ok {
			result = append(result, typed)
		}
	}
	return result
}

func extractQuotaAndRestoreAt(items []map[string]any) (int, string) {
	for _, item := range items {
		if strings.TrimSpace(stringValue(item["feature_name"])) != "image_gen" {
			continue
		}
		return intValue(item["remaining"]), stringValue(item["reset_after"])
	}
	return 0, ""
}

func hasLimitFeature(items []map[string]any, featureName string) bool {
	target := strings.TrimSpace(strings.ToLower(featureName))
	for _, item := range items {
		if strings.TrimSpace(strings.ToLower(stringValue(item["feature_name"]))) != target {
			continue
		}
		return true
	}
	return false
}

func stringOrDefault(data map[string]any, key, fallback string) string {
	if value := firstString(data, key); value != "" {
		return value
	}
	return fallback
}

func firstString(data map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(stringValue(data[key])); value != "" {
			return value
		}
	}
	return ""
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case fmt.Stringer:
		return typed.String()
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", value))
	}
}

func intValue(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int8:
		return int(typed)
	case int16:
		return int(typed)
	case int32:
		return int(typed)
	case int64:
		return int(typed)
	case float32:
		return int(typed)
	case float64:
		return int(typed)
	case json.Number:
		n, _ := typed.Int64()
		return int(n)
	case string:
		var n int
		fmt.Sscanf(strings.TrimSpace(typed), "%d", &n)
		return n
	default:
		return 0
	}
}
