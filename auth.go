package main

import (
	"crypto/hmac"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var reAnyCode = regexp.MustCompile(`TLM-[A-Z0-9]{6}`)

type verifyPage struct {
	Base
	Step      int
	Purpose   string // register | reset
	Code      string
	Texts     []string
	IntentURL string
	SiteBase  string
	TweetURL  string
	Err       string
	Prof      *xUser
	AvatarURL string
}

type verifyProfile struct {
	Name    string `json:"name"`
	Handle  string `json:"handle"`
	XID     string `json:"xid"`
	Avatar  string `json:"avatar"`
	TweetID string `json:"tweet_id"`
	UserID  int64  `json:"user_id,omitempty"`
}

func (a *App) tweetTexts(code, purpose string) []string {
	site := shortURL(a.cfg.BaseURL)
	if purpose == "reset" {
		return []string{"我在「推了么」的密码忘了，发帖证明是我本人 🔑 " + code + " " + site}
	}
	return []string{
		"我在「推了么」注册了，发帖验证本人 📣 " + code + " " + site,
		"发一条推就能接任务赚 U？来「推了么」试试 📣 验证码 " + code + " " + site,
		"推了么：X 发帖任务撮合，钱不过平台手 ✅ " + code + " " + site,
	}
}

// startVerify 生成验证码并渲染第一步。验证码与浏览器 cookie 里的密钥绑定：推文是公开的，光知道码没用。
func (a *App) startVerify(w http.ResponseWriter, r *http.Request, purpose, errMsg string) {
	if !a.lim.allow("newcode:"+a.ip(r), 30, 10*time.Minute) {
		a.errorPage(w, r, http.StatusTooManyRequests, "手速太快了", "歇一会儿再来。")
		return
	}
	a.st.PruneVerify()
	code := ""
	if pc := cookieVal(r, "xvp_"+purpose); pc != "" {
		if v, err := a.st.GetVerify(pc); err == nil && v != nil && !v.Used && v.Purpose == purpose && ms()-v.CreatedAt < 20*hourMs && ownsVerify(r, v) {
			code = v.Code
		}
	}
	if code == "" {
		code = "TLM-" + randCode(6)
		secret := randHex(16)
		if err := a.st.CreateVerify(code, purpose, hashToken(secret)); err != nil {
			a.fail(w, r, err)
			return
		}
		a.setCookie(w, "xv_"+code, secret, 24*3600)
		a.setCookie(w, "xvp_"+purpose, code, 24*3600)
	}
	texts := a.tweetTexts(code, purpose)
	if len(texts) > 1 {
		i := randInt(len(texts))
		texts[0], texts[i] = texts[i], texts[0]
	}
	a.render(w, http.StatusOK, "verify", verifyPage{Base: a.base(w, r), Step: 1, Purpose: purpose, Code: code, Texts: texts, SiteBase: shortURL(a.cfg.BaseURL), IntentURL: intentFor(texts[0]), Err: errMsg})
}

func ownsVerify(r *http.Request, v *Verify) bool {
	c := cookieVal(r, "xv_"+v.Code)
	return c != "" && v.Secret != "" && hmac.Equal([]byte(hashToken(c)), []byte(v.Secret))
}

func (a *App) step1(w http.ResponseWriter, r *http.Request, status int, purpose string, v *Verify, tweetURL, msg string) {
	texts := a.tweetTexts(v.Code, purpose)
	a.render(w, status, "verify", verifyPage{Base: a.base(w, r), Step: 1, Purpose: purpose, Code: v.Code, Texts: texts, SiteBase: shortURL(a.cfg.BaseURL), IntentURL: intentFor(texts[0]), TweetURL: tweetURL, Err: msg})
}

// verifyTweet 校验「验证码 + 推文链接」：读推文，正文须含验证码，返回作者资料。
func (a *App) verifyTweet(r *http.Request, purpose string) (*Verify, *Tweet, string) {
	code := strings.ToUpper(strings.TrimSpace(r.FormValue("code")))
	v, err := a.st.GetVerify(code)
	if err != nil || v == nil || v.Used || v.Purpose != purpose || ms()-v.CreatedAt > dayMs || !ownsVerify(r, v) {
		return nil, nil, "验证码已失效（或不是本浏览器生成的），请重新生成"
	}
	id := tweetIDFrom(r.FormValue("tweet_url"))
	if id == "" {
		return v, nil, "推文链接不对，应形如 https://x.com/你的名字/status/1234567890"
	}
	tw, err := a.fetchTweet(id, purpose)
	if err != nil {
		return v, nil, err.Error()
	}
	up := strings.ToUpper(tw.Text)
	if !strings.Contains(up, code) {
		found := false
		for _, c := range reAnyCode.FindAllString(up, -1) {
			if c == code {
				continue
			}
			if v2, err := a.st.GetVerify(c); err == nil && v2 != nil && !v2.Used && v2.Purpose == purpose && ms()-v2.CreatedAt < dayMs && ownsVerify(r, v2) {
				v, code, found = v2, c, true
				break
			}
		}
		if !found {
			return v, nil, "这条推文里没有验证码 " + code + "，请检查是否发对了"
		}
	}
	if tw.CreatedMs > 0 && tw.CreatedMs < v.CreatedAt-10*60*1000 {
		return v, nil, "这条推文比验证码还早，请用生成验证码之后发的推文"
	}
	return v, tw, ""
}

func (a *App) loadVerified(r *http.Request, purpose string) (*Verify, *verifyProfile, string) {
	code := strings.ToUpper(strings.TrimSpace(r.FormValue("code")))
	v, err := a.st.GetVerify(code)
	if err != nil || v == nil || v.Used || v.Purpose != purpose || v.Profile == "" || ms()-v.CreatedAt > dayMs || !ownsVerify(r, v) {
		return nil, nil, "验证已失效，请重新验证"
	}
	var prof verifyProfile
	if err := json.Unmarshal([]byte(v.Profile), &prof); err != nil || prof.Handle == "" {
		return nil, nil, "验证已失效，请重新验证"
	}
	return v, &prof, ""
}

func bigAvatar(u string) string { return strings.Replace(u, "_normal", "_200x200", 1) }

// ---------- 注册 ----------

func (a *App) handleRegisterGet(w http.ResponseWriter, r *http.Request) {
	if a.currentUser(r) != nil {
		http.Redirect(w, r, "/me", http.StatusFound)
		return
	}
	a.startVerify(w, r, "register", "")
}

func (a *App) handleRegisterVerify(w http.ResponseWriter, r *http.Request) {
	if a.limited(w, r, "verify", 30, 10*time.Minute) {
		return
	}
	v, tw, msg := a.verifyTweet(r, "register")
	if msg != "" {
		if v == nil {
			a.startVerify(w, r, "register", msg)
			return
		}
		a.step1(w, r, http.StatusBadRequest, "register", v, r.FormValue("tweet_url"), msg)
		return
	}
	if exist, _ := a.st.GetUserByXID(tw.User.ID); exist != nil {
		a.step1(w, r, http.StatusBadRequest, "register", v, "", "@"+tw.User.Handle+" 已经注册过了，请直接登录；忘了密码可以用 X 找回。")
		return
	}
	if a.st.BlacklistedIdentity(tw.User.ID, "", "") {
		a.step1(w, r, http.StatusForbidden, "register", v, "", "这个 X 账号在黑名单上，不能注册。")
		return
	}
	prof := verifyProfile{Name: cleanText(tw.User.Name, 30, false), Handle: tw.User.Handle, XID: tw.User.ID, Avatar: bigAvatar(tw.User.Avatar), TweetID: tw.ID}
	pj, _ := json.Marshal(prof)
	if err := a.st.SetVerifyProfile(v.Code, string(pj)); err != nil {
		a.fail(w, r, err)
		return
	}
	a.render(w, http.StatusOK, "verify", verifyPage{Base: a.base(w, r), Step: 2, Purpose: "register", Code: v.Code, Prof: &xUser{Name: prof.Name, Handle: prof.Handle, ID: prof.XID}, AvatarURL: prof.Avatar})
}

func (a *App) handleRegisterPost(w http.ResponseWriter, r *http.Request) {
	if a.limited(w, r, "register", 15, 10*time.Minute) {
		return
	}
	v, prof, msg := a.loadVerified(r, "register")
	if msg != "" {
		a.startVerify(w, r, "register", msg)
		return
	}
	bad := func(m string) {
		a.render(w, http.StatusBadRequest, "verify", verifyPage{Base: a.base(w, r), Step: 2, Purpose: "register", Code: v.Code, Prof: &xUser{Name: prof.Name, Handle: prof.Handle, ID: prof.XID}, AvatarURL: prof.Avatar, Err: m})
	}
	pw, pw2 := r.FormValue("password"), r.FormValue("password2")
	if len(pw) < 8 || len(pw) > 72 {
		bad("密码至少 8 位")
		return
	}
	if pw != pw2 {
		bad("两次密码不一致")
		return
	}
	u := &User{XID: prof.XID, Handle: prof.Handle, DisplayName: prof.Name, AvatarURL: prof.Avatar, PassHash: hashPassword(pw), RegTweetID: prof.TweetID, XCreatedMs: xCreatedMs(prof.XID)}
	id, err := a.st.CreateUser(u)
	if errors.Is(err, ErrXTaken) {
		bad("@" + prof.Handle + " 已经注册过了")
		return
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.st.UseVerify(v.Code)
	a.st.Audit(id, "user.register", "user", id, map[string]any{"tweet": prof.TweetID}, a.ip(r))
	a.logf("[info] 新用户 #%d @%s", id, prof.Handle)
	if created, err := a.st.GetUserByID(id); err == nil && created != nil {
		a.login(w, created)
		a.spawn("followers", func() { a.refreshFollowers(created, true) })
	}
	a.flash(w, "注册成功。先绑定收款方式，接单和发布都需要它。")
	http.Redirect(w, r, "/me/pay", http.StatusFound)
}

// ---------- 找回密码 ----------

func (a *App) handleResetGet(w http.ResponseWriter, r *http.Request) {
	a.startVerify(w, r, "reset", "")
}

func (a *App) handleResetVerify(w http.ResponseWriter, r *http.Request) {
	if a.limited(w, r, "verify", 30, 10*time.Minute) {
		return
	}
	v, tw, msg := a.verifyTweet(r, "reset")
	if msg != "" {
		if v == nil {
			a.startVerify(w, r, "reset", msg)
			return
		}
		a.step1(w, r, http.StatusBadRequest, "reset", v, r.FormValue("tweet_url"), msg)
		return
	}
	u, _ := a.st.GetUserByXID(tw.User.ID) // 只按 X 数字 ID 认人，绝不按用户名回退
	if u == nil {
		a.step1(w, r, http.StatusBadRequest, "reset", v, "", "@"+tw.User.Handle+" 还没注册过")
		return
	}
	if !a.lim.allow("reset:"+strconv.FormatInt(u.ID, 10), 3, 24*time.Hour) {
		a.step1(w, r, http.StatusTooManyRequests, "reset", v, "", "这个账号今天找回得太频繁了，明天再来")
		return
	}
	a.st.SyncX(u.ID, tw.User.Handle, cleanText(tw.User.Name, 30, false), bigAvatar(tw.User.Avatar))
	pj, _ := json.Marshal(verifyProfile{Name: tw.User.Name, Handle: tw.User.Handle, XID: tw.User.ID, UserID: u.ID, TweetID: tw.ID})
	a.st.SetVerifyProfile(v.Code, string(pj))
	a.render(w, http.StatusOK, "verify", verifyPage{Base: a.base(w, r), Step: 2, Purpose: "reset", Code: v.Code, Prof: &xUser{Name: tw.User.Name, Handle: tw.User.Handle, ID: tw.User.ID}, AvatarURL: bigAvatar(tw.User.Avatar)})
}

func (a *App) handleResetPost(w http.ResponseWriter, r *http.Request) {
	v, prof, msg := a.loadVerified(r, "reset")
	if msg != "" || prof.UserID == 0 {
		a.startVerify(w, r, "reset", "验证已失效，请重新验证")
		return
	}
	u, err := a.st.GetUserByID(prof.UserID)
	if err != nil || u == nil || u.XID != prof.XID {
		a.startVerify(w, r, "reset", "验证已失效，请重新验证")
		return
	}
	pw, pw2 := r.FormValue("password"), r.FormValue("password2")
	if len(pw) < 8 || len(pw) > 72 || pw != pw2 {
		a.render(w, http.StatusBadRequest, "verify", verifyPage{Base: a.base(w, r), Step: 2, Purpose: "reset", Code: v.Code, Prof: &xUser{Name: prof.Name, Handle: prof.Handle}, AvatarURL: u.AvatarURL, Err: "密码至少 8 位，且两次一致"})
		return
	}
	if ok, _ := a.st.UseVerify(v.Code); !ok {
		a.startVerify(w, r, "reset", "验证已失效，请重新验证")
		return
	}
	if err := a.st.SetPassword(u.ID, hashPassword(pw)); err != nil {
		a.fail(w, r, err)
		return
	}
	a.st.Audit(u.ID, "user.reset_password", "user", u.ID, nil, a.ip(r))
	if fresh, err := a.st.GetUserByID(u.ID); err == nil && fresh != nil {
		a.login(w, fresh)
	}
	a.flash(w, "密码已重置，其它设备上的登录已失效")
	http.Redirect(w, r, "/me", http.StatusFound)
}

// ---------- 登录 ----------

type loginPage struct {
	Base
	Login string
	Next  string
	Err   string
}

func safeNext(s string) string {
	if s == "" || !strings.HasPrefix(s, "/") || strings.HasPrefix(s, "//") || strings.ContainsAny(s, "\\\r\n") {
		return "/me"
	}
	if u, err := url.Parse(s); err != nil || u.Host != "" || u.Scheme != "" || u.User != nil {
		return "/me"
	}
	return s
}

func (a *App) handleLoginGet(w http.ResponseWriter, r *http.Request) {
	if a.currentUser(r) != nil {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusFound)
		return
	}
	a.render(w, http.StatusOK, "login", loginPage{Base: a.base(w, r), Next: safeNext(r.URL.Query().Get("next"))})
}

func (a *App) handleLoginPost(w http.ResponseWriter, r *http.Request) {
	if a.limited(w, r, "login", 15, time.Minute) {
		return
	}
	login := strings.TrimPrefix(strings.TrimSpace(r.FormValue("login")), "@")
	pw := r.FormValue("password")
	next := safeNext(r.FormValue("next"))
	p := loginPage{Base: a.base(w, r), Login: login, Next: next, Err: "X 用户名或密码不对"}
	key := "loginfail:" + a.ip(r) + ":" + strings.ToLower(login)
	if a.lim.count(key, 10*time.Minute) >= 10 {
		a.errorPage(w, r, http.StatusTooManyRequests, "手速太快了", "这个账号在你这里错误次数太多，10 分钟后再试；忘了密码可以用 X 找回。")
		return
	}
	u, err := a.st.GetUserByLogin(login)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if u == nil || u.Status == "banned" || !checkPassword(u.PassHash, pw) {
		a.lim.allow(key, 1000, 10*time.Minute)
		a.render(w, http.StatusUnauthorized, "login", p)
		return
	}
	a.login(w, u)
	a.st.TouchIP(u.ID, a.ip(r))
	http.Redirect(w, r, next, http.StatusFound)
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	a.logout(w)
	http.Redirect(w, r, "/", http.StatusFound)
}

// handlePassword 已登录改密码。
func (a *App) handlePassword(w http.ResponseWriter, r *http.Request) {
	u, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	if a.limited(w, r, "pw", 10, 10*time.Minute) {
		return
	}
	old, pw, pw2 := r.FormValue("old"), r.FormValue("password"), r.FormValue("password2")
	switch {
	case !checkPassword(u.PassHash, old):
		a.flash(w, "原密码不对")
	case len(pw) < 8 || len(pw) > 72:
		a.flash(w, "新密码至少 8 位")
	case pw != pw2:
		a.flash(w, "两次密码不一致")
	default:
		if err := a.st.SetPassword(u.ID, hashPassword(pw)); err != nil {
			a.fail(w, r, err)
			return
		}
		if fresh, err := a.st.GetUserByID(u.ID); err == nil && fresh != nil {
			a.login(w, fresh)
		}
		a.st.Audit(u.ID, "user.change_password", "user", u.ID, nil, a.ip(r))
		a.flash(w, "密码已修改，其它设备上的登录已失效")
	}
	http.Redirect(w, r, "/me", http.StatusFound)
}
