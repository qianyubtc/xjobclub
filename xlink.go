package main

import (
	"encoding/json"
	"net"
	"net/http"
	"time"
)

// 按 X 账号互通（2026-10-07）：芊羽的其他产品（蓝不住）按 X 用户名问"这个人在推了么有没有主页"。
//
//	GET /api/x/{handle}  {"handle","url","xid"}；没有 url 为 null
//
// 用户名被别人顶替过的旧号（HandleStale）不给，免得把别人的主页挂到这个用户名下；黑名单 / 封禁的也不往外推
// （蓝不住那边也不把挂人的榜推给别的产品，两边一个规矩）。回 X 数字 ID：对方拿去和自己记的比，对不上就不挂。
func (a *App) handleXAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if !directLocal(r, a.cfg.TrustHeader) && !a.lim.allow("xapi:"+a.ip(r), 120, time.Minute) {
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]any{"error": "too many requests"})
		return
	}
	h := normHandle(r.PathValue("handle"))
	out := map[string]any{"handle": h, "url": nil}
	if h != "" {
		u, err := a.st.GetUserByHandle(h)
		if err != nil { // 查库出错别说成「没有这个人」：对方会把「没有」缓存很久
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]any{"error": "unavailable"})
			return
		}
		if u != nil && !u.HandleStale && !u.Blacklisted() {
			out["handle"], out["url"], out["xid"] = u.Handle, a.cfg.BaseURL+u.Path(), u.XID
		}
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	json.NewEncoder(w).Encode(out)
}

// directLocal 同一台机器上的程序直接来问（蓝不住走 127.0.0.1，没经过 Caddy，不带转发头）：不限流，
// 免得和绕过 Cloudflare 直连源站的请求挤在同一个 127.0.0.1 的桶里被挤爆。经 Caddy 进来的请求一定带 X-Forwarded-For。
func directLocal(r *http.Request, trustHeader string) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return false
	}
	for _, h := range []string{"X-Forwarded-For", "X-Real-IP", "CF-Connecting-IP", trustHeader} {
		if h != "" && r.Header.Get(h) != "" {
			return false
		}
	}
	return true
}
