package bingx

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// signedRequest performs a BingX signed REST call.
//
// Per BingX docs: every signed endpoint requires `timestamp` (ms) in the
// query string, and a `signature` parameter which is HMAC-SHA256(secret,
// canonical_query_string). The canonical query is the same query string
// the request will send, ordered as-set (BingX does NOT require sorting,
// but we sort for determinism). The signature is appended last.
//
// method: "GET" / "POST" / "DELETE"
// path:   the API path, e.g. "/openApi/swap/v2/user/positions"
// params: caller-supplied query parameters (we add timestamp + signature)
// out:    JSON-decoded into this if non-nil
//
// All BingX private endpoints return a top-level envelope:
//
//	{"code":0,"msg":"","data":{...}}
//
// We surface non-zero `code` as an error so callers don't have to
// re-check it on every call.
func (c *Client) signedRequest(ctx context.Context, method, path string, params url.Values, out any) error {
	if c.APIKey == "" || c.APISecret == "" {
		return fmt.Errorf("bingx: signed endpoint %s requires APIKey + APISecret", path)
	}
	if params == nil {
		params = url.Values{}
	}
	params.Set("timestamp", strconv.FormatInt(time.Now().UnixMilli(), 10))

	// BingX docs: signature = HMAC-SHA256(secret, queryString). They don't
	// require sorted keys, but url.Values.Encode() sorts alphabetically
	// which gives us a stable canonical form for debugging.
	canonical := params.Encode()
	mac := hmac.New(sha256.New, []byte(c.APISecret))
	mac.Write([]byte(canonical))
	sig := hex.EncodeToString(mac.Sum(nil))

	endpoint := c.Host + path + "?" + canonical + "&signature=" + sig

	var body io.Reader
	// BingX accepts the signed params in the query string for ALL methods,
	// including POST (it doesn't require form bodies). Keeping params in
	// the URL means we sign exactly what we send.
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return fmt.Errorf("build req: %w", err)
	}
	req.Header.Set("X-BX-APIKEY", c.APIKey)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("do: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http %d: %s", resp.StatusCode, raw)
	}

	// Peek at the envelope to surface BingX-side errors (e.g. wrong IP,
	// missing trade scope) as Go errors instead of confusing decode misses.
	var env struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode envelope: %w (body=%s)", err, raw)
	}
	if env.Code != 0 {
		return fmt.Errorf("bingx code=%d msg=%q (path=%s)", env.Code, env.Msg, path)
	}
	if out == nil || len(env.Data) == 0 {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("decode data: %w (body=%s)", err, env.Data)
	}
	return nil
}
