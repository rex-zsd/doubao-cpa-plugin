package doubao

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"rsc.io/qr"
)

// The live Passport service returns an absolute Unix timestamp; accept TTLs
// too so display metadata does not confuse an epoch value with a duration.
func qrExpiry(raw any, now time.Time) (time.Time, int) {
	n := int64(number(raw))
	var deadline time.Time
	switch {
	case n > 1_000_000_000_000:
		deadline = time.UnixMilli(n)
	case n >= 1_000_000_000:
		deadline = time.Unix(n, 0)
	case n > 0:
		deadline = now.Add(time.Duration(n) * time.Second)
	default:
		deadline = now.Add(time.Minute)
	}
	remaining := int(deadline.Sub(now).Seconds())
	if remaining < 0 {
		remaining = 0
	}
	return deadline, remaining
}

// writeQRPNG renders the QR in-process, so login works in minimal CPA
// containers without an external qrencode binary.
func writeQRPNG(path, text string) error {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return errors.New("QR image generation failed")
	}
	if err = os.WriteFile(path, code.PNG(), 0600); err != nil {
		return err
	}
	return os.Chmod(path, 0600)
}

func qrData(v object) (any, error) {
	d := field(v, "data")
	if d == nil {
		return nil, errors.New("QR response has no data")
	}
	code := number(field(d, "error_code"))
	if code == 0 {
		code = number(field(v, "error_code"))
	}
	if truthy(field(d, "captcha")) || code == 22 {
		return nil, errors.New("login_action_required: complete the security check in the supported app/browser")
	}
	if code != 0 {
		return nil, fmt.Errorf("QR request rejected, code=%d", code)
	}
	return d, nil
}
func (c *Client) login(ctx context.Context) error {
	r, err := c.request(ctx, "GET", "/", nil, nil)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, io.LimitReader(r.Body, 8<<20))
	r.Body.Close()
	v, err := c.json(ctx, "GET", "/passport/web/get_qrcode/", url.Values{"next": {origin}}, nil)
	if err != nil {
		return err
	}
	d, err := qrData(v)
	if err != nil {
		return err
	}
	qrToken := textValue(field(d, "token"))
	qrURL := textValue(field(d, "qrcode_index_url"))
	if qrURL == "" {
		qrURL = textValue(field(d, "qrcode"))
	}
	if qrToken == "" || qrURL == "" {
		return errors.New("QR response missing token or URL")
	}
	qrPath := filepath.Join(c.dir, "login-qr.png")
	if err := writeQRPNG(qrPath, qrURL); err != nil {
		return err
	}
	issuedAt := time.Now()
	deadline, expires := qrExpiry(field(d, "expire_time"), issuedAt)
	statusRecord := object{"status": "qr_ready", "url": qrURL, "issued_at": issuedAt.Format(time.RFC3339), "expires_in": expires, "expires_at": deadline.Format(time.RFC3339), "qr_path": qrPath}
	if err := saveJSON(filepath.Join(c.dir, "login-status.json"), statusRecord); err != nil {
		return err
	}
	fmt.Println("QR_READY", qrPath)
	last := ""
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		v, err := c.json(ctx, "GET", "/passport/web/check_qrconnect/", url.Values{"next": {origin}, "token": {qrToken}}, nil)
		if err != nil {
			return err
		}
		d, err := qrData(v)
		if err != nil {
			return err
		}
		state := textValue(field(d, "status"))
		if state != last {
			fmt.Println("QR_STATE", state)
			last = state
			statusRecord["status"] = state
			statusRecord["updated_at"] = time.Now().Format(time.RFC3339)
			if err := saveJSON(filepath.Join(c.dir, "login-status.json"), statusRecord); err != nil {
				return err
			}
		}
		switch state {
		case "new", "scanned":
		case "confirmed":
			for _, key := range []string{"redirect_url", "redirectUrl", "redirect", "url", "next"} {
				next := textValue(field(d, key))
				if next == "" {
					next = textValue(field(d, "extra", key))
				}
				if next == "" {
					continue
				}
				for n := 0; n < 5; n++ {
					r, err := c.request(ctx, "GET", next, nil, nil)
					if err != nil {
						return err
					}
					io.Copy(io.Discard, io.LimitReader(r.Body, 8<<20))
					r.Body.Close()
					if r.StatusCode < 300 || r.StatusCode >= 400 {
						next = ""
						break
					}
					next = r.Header.Get("Location")
					if next == "" {
						return errors.New("login redirect missing location")
					}
				}
				if next != "" {
					return errors.New("login redirect limit exceeded")
				}
				break
			}
			if err := c.account(ctx); err != nil {
				return err
			}
			fmt.Println("LOGIN_VERIFIED")
			return saveJSON(filepath.Join(c.dir, "login-status.json"), object{"status": "verified", "verified_at": time.Now().Format(time.RFC3339)})
		case "expired":
			return errors.New("qr_expired")
		case "cancelled", "canceled", "rejected":
			return errors.New("qr_cancelled")
		default:
			return errors.New("unsupported QR state")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
