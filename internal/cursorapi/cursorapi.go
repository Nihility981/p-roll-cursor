// Package cursorapi 封装 Cursor 的四个外部 HTTP 接口。
// 常量与 Header 组合来自 Rust 参照实现，原样保留。
package cursorapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	UsageSummaryURL      = "https://cursor.com/api/usage-summary"
	GetUserMetaURL       = "https://api2.cursor.sh/aiserver.v1.AuthService/GetUserMeta"
	FullStripeProfileURL = "https://api2.cursor.sh/auth/full_stripe_profile"
	StripeProfileURL     = "https://api2.cursor.sh/auth/stripe_profile"

	// 与 Rust 实现一致的浏览器 UA，cursor.com 的网页接口对 UA 敏感。
	browserUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)"

	defaultTimeout = 15 * time.Second
)

// Client 是所有 Cursor 接口共用的 HTTP 客户端。
type Client struct {
	http *http.Client
}

// New 创建一个统一 15s 超时的客户端。
func New() *Client {
	return &Client{http: &http.Client{Timeout: defaultTimeout}}
}

// Result 保存一次请求的原始结果，便于探测阶段完整留档。
type Result struct {
	URL        string
	Method     string
	StatusCode int
	Status     string
	Body       []byte
	Duration   time.Duration
	// ContentType 用于判断响应到底是 JSON 还是被重定向成了 HTML 登录页。
	ContentType string
}

// IsJSON 粗略判断响应体是否为 JSON。
// 401 时 cursor.com 会返回 HTML，靠状态码不足以区分「未认证」和「接口变更」。
func (r *Result) IsJSON() bool {
	if strings.Contains(strings.ToLower(r.ContentType), "json") {
		return true
	}
	trimmed := bytes.TrimSpace(r.Body)
	return len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[')
}

// BodySnippet 返回截断后的响应体，用于错误诊断输出。
func (r *Result) BodySnippet(n int) string {
	s := strings.TrimSpace(string(r.Body))
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", "")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func (c *Client) do(ctx context.Context, req *http.Request) (*Result, error) {
	started := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 %s 失败: %w", req.URL.String(), err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 限制读取上限，避免异常响应把内存吃满。
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("读取 %s 响应体失败: %w", req.URL.String(), err)
	}

	return &Result{
		URL:         req.URL.String(),
		Method:      req.Method,
		StatusCode:  resp.StatusCode,
		Status:      resp.Status,
		Body:        body,
		Duration:    time.Since(started),
		ContentType: resp.Header.Get("Content-Type"),
	}, nil
}

// ---------------------------------------------------------------------------
// GetUserMeta
// ---------------------------------------------------------------------------

// UserMeta 是 GetUserMeta 的响应，字段为 camelCase。
type UserMeta struct {
	Email      string `json:"email"`
	SignUpType string `json:"signUpType"`
	WorkosID   string `json:"workosId"`
}

// GetUserMeta 以 Bearer token 调用 AuthService/GetUserMeta，body 固定为 {}。
func (c *Client) GetUserMeta(ctx context.Context, accessToken string) (*Result, *UserMeta, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, GetUserMetaURL,
		strings.NewReader("{}"))
	if err != nil {
		return nil, nil, fmt.Errorf("构造 GetUserMeta 请求失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	res, err := c.do(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	if res.StatusCode != http.StatusOK {
		return res, nil, nil
	}

	var meta UserMeta
	if err := json.Unmarshal(res.Body, &meta); err != nil {
		return res, nil, fmt.Errorf("解析 GetUserMeta JSON 失败: %w", err)
	}
	return res, &meta, nil
}

// ---------------------------------------------------------------------------
// Stripe profile
// ---------------------------------------------------------------------------

// StripeProfile 是 full_stripe_profile / stripe_profile 的响应。
type StripeProfile struct {
	MembershipType           string `json:"membershipType"`
	IndividualMembershipType string `json:"individualMembershipType"`
	SubscriptionStatus       string `json:"subscriptionStatus"`
	TeamMembershipType       string `json:"teamMembershipType"`
	IsTeamMember             bool   `json:"isTeamMember"`
	IsEnterprise             bool   `json:"isEnterprise"`
}

// StripeProfileOutcome 记录两个端点的尝试过程。
type StripeProfileOutcome struct {
	Full     *Result
	Fallback *Result
	Profile  *StripeProfile
	// UsedURL 是最终成功的端点，均失败时为空。
	UsedURL string
}

// FetchStripeProfile 先试 full_stripe_profile，非 200 再回退 stripe_profile。
func (c *Client) FetchStripeProfile(ctx context.Context, accessToken string) (*StripeProfileOutcome, error) {
	outcome := &StripeProfileOutcome{}

	full, err := c.getWithBearer(ctx, FullStripeProfileURL, accessToken)
	if err != nil {
		return outcome, err
	}
	outcome.Full = full
	if full.StatusCode == http.StatusOK {
		profile, err := decodeStripeProfile(full.Body)
		if err != nil {
			return outcome, err
		}
		outcome.Profile = profile
		outcome.UsedURL = FullStripeProfileURL
		return outcome, nil
	}

	fallback, err := c.getWithBearer(ctx, StripeProfileURL, accessToken)
	if err != nil {
		return outcome, err
	}
	outcome.Fallback = fallback
	if fallback.StatusCode == http.StatusOK {
		profile, err := decodeStripeProfile(fallback.Body)
		if err != nil {
			return outcome, err
		}
		outcome.Profile = profile
		outcome.UsedURL = StripeProfileURL
	}
	return outcome, nil
}

func decodeStripeProfile(body []byte) (*StripeProfile, error) {
	var profile StripeProfile
	if err := json.Unmarshal(body, &profile); err != nil {
		return nil, fmt.Errorf("解析 stripe profile JSON 失败: %w", err)
	}
	return &profile, nil
}

func (c *Client) getWithBearer(ctx context.Context, url, accessToken string) (*Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("构造请求 %s 失败: %w", url, err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	return c.do(ctx, req)
}

// ---------------------------------------------------------------------------
// usage-summary
// ---------------------------------------------------------------------------

// BuildSessionCookie 构造 WorkosCursorSessionToken。
//
// 分隔符必须是字面量 "%3A%3A"（即已 URL 编码的 "::"）。Cookie 值不会被
// Go 再次编码，所以这里写死编码后的形式才是服务端期望的。
func BuildSessionCookie(userID, accessToken string) string {
	return fmt.Sprintf("WorkosCursorSessionToken=%s%%3A%%3A%s", userID, accessToken)
}

// UsageSummaryOptions 控制一次 usage-summary 探测请求的认证方式。
// PoC 阶段需要横向比较多种候选组合，所以把它做成显式选项。
type UsageSummaryOptions struct {
	// CookieUserID 非空时携带 WorkosCursorSessionToken cookie。
	CookieUserID string
	// AccessToken 既用于 cookie 后半段，也可单独作为 Bearer。
	AccessToken string
	// WithBearer 为 true 时额外带 Authorization: Bearer。
	WithBearer bool
}

// FetchUsageSummary 按给定认证组合请求 usage-summary。
func (c *Client) FetchUsageSummary(ctx context.Context, opts UsageSummaryOptions) (*Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, UsageSummaryURL, nil)
	if err != nil {
		return nil, fmt.Errorf("构造 usage-summary 请求失败: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", browserUserAgent)
	if opts.CookieUserID != "" {
		req.Header.Set("Cookie", BuildSessionCookie(opts.CookieUserID, opts.AccessToken))
	}
	if opts.WithBearer {
		req.Header.Set("Authorization", "Bearer "+opts.AccessToken)
	}
	return c.do(ctx, req)
}
