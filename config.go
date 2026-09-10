package main

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// Config 全部来自 config.env（KEY=VALUE）或同名环境变量（环境变量优先）。
type Config struct {
	Listen    string
	BaseURL   string
	DBPath    string
	UploadDir string
	SiteTitle string
	RepoURL   string
	AuthorX   string

	// BinancePayTool 网关（必填：没有网关就没有自动到账，也没有认证付款）
	BPGURL      string
	BPGKey      string
	Currency    string
	OrderTTL    int64  // 网关结账会话秒数
	CallbackURL string // 网关回调地址（缺省按 LISTEN 拼内网地址）

	// 管理员：X 用户名列表（小写），用自己的账号登录即有后台权限
	AdminHandles map[string]bool
	AdminIPAllow []*net.IPNet

	TrustProxy  bool
	TrustHeader string
	XTweetAPI   string // 推文抓取接口基址（测试时指向 mock）
	XSyndAPI    string // X 嵌入时间线接口（粉丝数第一来源）
	XProfileAPI string // FxTwitter 风格的公开镜像接口（粉丝数第二来源）

	// 任务参数
	MinRewardE8      int64
	MaxRewardE8      int64
	MaxSlots         int64
	ClaimTTLDefault  int64 // 分钟
	ClaimTTLMin      int64
	ClaimTTLMax      int64
	RetentionOptions []int64 // 小时
	RetentionDefault int64
	PayWindowOptions []int64 // 小时
	PayWindowDefault int64
	DeadlineDefault  int64 // 天
	DeadlineMax      int64
	MaxVariants      int64
	VerifyAttempts   int64

	// 履约与纠纷时限
	GraceReportH    int64 // 举报后宽限
	EvidenceWindowH int64
	RecheckUnknownH int64 // 复检无结论多久后判通过
	VoidStrikes     int64 // 30 天内留存不达标几次暂停接单
	VoidSuspendDays int64
	ConfirmRemindH  []int64

	// 信用额度（单位 U，内部 e8）
	ExposureNewbieE8  int64
	ExposureRegularE8 int64
	ExposureSeniorE8  int64
	OpenTasksNewbie   int64
	OpenTasksRegular  int64
	OpenTasksSenior   int64
	ConcurNewbie      int64
	ConcurRegular     int64
	ConcurSenior      int64
	DailyNewbie       int64
	DailyRegular      int64
	DailySenior       int64

	// 发布方认证付款
	CertFeeEnabled bool
	CertFeeAmount  string
	CertFeeE8      int64

	// 小法庭
	JuryEnabled bool
	JuryMinPool int64
	JuryPanel   int64
	JuryInvite  int64
	JuryTypes   map[string]bool
	JuryWindowH int64
	JuryAppealH int64
}

func loadConfig(path string) (*Config, error) {
	vals := map[string]string{}
	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			v = strings.TrimSpace(v)
			if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
				v = v[1 : len(v)-1]
			}
			vals[strings.TrimSpace(k)] = v
		}
		f.Close()
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	get := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		if v, ok := vals[k]; ok && v != "" {
			return v
		}
		return def
	}
	getBool := func(k string, def bool) bool {
		v := strings.ToLower(get(k, ""))
		if v == "" {
			return def
		}
		return v == "1" || v == "true" || v == "yes" || v == "on"
	}
	var firstErr error
	getInt := func(k string, def int64) int64 {
		v := get(k, "")
		if v == "" {
			return def
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s 须为整数: %q", k, v)
		}
		return n
	}
	getList := func(k, def string) []int64 {
		var out []int64
		for _, p := range strings.Split(get(k, def), ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			n, err := strconv.ParseInt(p, 10, 64)
			if err != nil && firstErr == nil {
				firstErr = fmt.Errorf("%s 含非整数 %q", k, p)
			}
			out = append(out, n)
		}
		return out
	}
	getU := func(k, def string) int64 {
		e8, err := parseAmountE8(get(k, def), 8)
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", k, err)
		}
		return e8
	}

	c := &Config{
		Listen:      get("LISTEN", "127.0.0.1:8125"),
		BaseURL:     strings.TrimRight(get("BASE_URL", "http://127.0.0.1:8125"), "/"),
		DBPath:      get("DB_PATH", "./xjobclub.db"),
		UploadDir:   get("UPLOAD_DIR", "./uploads"),
		SiteTitle:   get("SITE_TITLE", "推了么"),
		RepoURL:     get("REPO_URL", "https://github.com/qianyubtc/xjobclub"),
		AuthorX:     get("AUTHOR_X", "https://x.com/qianyuwing"),
		BPGURL:      strings.TrimRight(get("BPG_URL", ""), "/"),
		BPGKey:      get("BPG_KEY", ""),
		Currency:    strings.ToUpper(get("CURRENCY", "USDT")),
		CallbackURL: strings.TrimSpace(get("CALLBACK_URL", "")),
		TrustProxy:  getBool("TRUST_PROXY", false),
		TrustHeader: get("TRUST_PROXY_HEADER", "X-Forwarded-For"),
		XTweetAPI:   strings.TrimRight(get("X_TWEET_API", "https://cdn.syndication.twimg.com"), "/"),
		XSyndAPI:    strings.TrimRight(get("X_SYND_API", "https://syndication.twitter.com"), "/"),
		XProfileAPI: strings.TrimRight(get("X_PROFILE_API", "https://api.fxtwitter.com"), "/"),

		AdminHandles: map[string]bool{},
		JuryTypes:    map[string]bool{},
	}
	for _, h := range strings.Split(get("ADMIN_HANDLES", ""), ",") {
		h = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(h), "@")))
		if h != "" {
			c.AdminHandles[h] = true
		}
	}
	for _, cidr := range strings.Split(get("ADMIN_IP_ALLOW", ""), ",") {
		cidr = strings.TrimSpace(cidr)
		if cidr == "" {
			continue
		}
		if !strings.Contains(cidr, "/") {
			if strings.Contains(cidr, ":") {
				cidr += "/128"
			} else {
				cidr += "/32"
			}
		}
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("ADMIN_IP_ALLOW 含非法网段 %q", cidr)
		}
		c.AdminIPAllow = append(c.AdminIPAllow, n)
	}
	for _, t := range strings.Split(get("JURY_TYPES", "B,D"), ",") {
		if t = strings.ToUpper(strings.TrimSpace(t)); t != "" {
			c.JuryTypes[t] = true
		}
	}

	c.OrderTTL = getInt("ORDER_TTL", 1800)
	c.MinRewardE8 = getU("MIN_REWARD", "0.1")
	c.MaxRewardE8 = getU("MAX_REWARD", "500")
	c.MaxSlots = getInt("MAX_SLOTS", 100)
	c.ClaimTTLDefault = getInt("CLAIM_TTL_MIN", 120)
	c.ClaimTTLMin = getInt("CLAIM_TTL_MIN_MIN", 30)
	c.ClaimTTLMax = getInt("CLAIM_TTL_MAX_MIN", 1440)
	c.RetentionOptions = getList("RETENTION_OPTIONS", "0,24,72,168")
	c.RetentionDefault = getInt("RETENTION_DEFAULT_H", 24)
	c.PayWindowOptions = getList("PAY_WINDOW_OPTIONS", "24,48,72")
	c.PayWindowDefault = getInt("PAY_WINDOW_DEFAULT_H", 48)
	c.DeadlineDefault = getInt("DEADLINE_DEFAULT_DAYS", 7)
	c.DeadlineMax = getInt("DEADLINE_MAX_DAYS", 30)
	c.MaxVariants = getInt("MAX_VARIANTS", 5)
	c.VerifyAttempts = getInt("VERIFY_ATTEMPTS", 3)
	c.GraceReportH = getInt("GRACE_REPORT_H", 24)
	c.EvidenceWindowH = getInt("EVIDENCE_WINDOW_H", 72)
	c.RecheckUnknownH = getInt("RECHECK_UNKNOWN_H", 12)
	c.VoidStrikes = getInt("VOID_STRIKES", 3)
	c.VoidSuspendDays = getInt("VOID_SUSPEND_DAYS", 7)
	c.ConfirmRemindH = getList("CONFIRM_REMIND_H", "24,72")
	c.ExposureNewbieE8 = getU("EXPOSURE_NEWBIE", "20")
	c.ExposureRegularE8 = getU("EXPOSURE_REGULAR", "200")
	c.ExposureSeniorE8 = getU("EXPOSURE_SENIOR", "2000")
	c.OpenTasksNewbie = getInt("OPEN_TASKS_NEWBIE", 1)
	c.OpenTasksRegular = getInt("OPEN_TASKS_REGULAR", 3)
	c.OpenTasksSenior = getInt("OPEN_TASKS_SENIOR", 10)
	c.ConcurNewbie = getInt("CONCUR_NEWBIE", 2)
	c.ConcurRegular = getInt("CONCUR_REGULAR", 5)
	c.ConcurSenior = getInt("CONCUR_SENIOR", 10)
	c.DailyNewbie = getInt("DAILY_NEWBIE", 5)
	c.DailyRegular = getInt("DAILY_REGULAR", 20)
	c.DailySenior = getInt("DAILY_SENIOR", 50)
	c.CertFeeEnabled = getBool("CERT_FEE_ENABLED", true)
	c.CertFeeAmount = get("CERT_FEE_AMOUNT", "0.1")
	c.CertFeeE8 = getU("CERT_FEE_AMOUNT", "0.1")
	c.JuryEnabled = getBool("JURY_ENABLED", true)
	c.JuryMinPool = getInt("JURY_MIN_POOL", 50)
	c.JuryPanel = getInt("JURY_PANEL", 7)
	c.JuryInvite = getInt("JURY_INVITE", 21)
	c.JuryWindowH = getInt("JURY_WINDOW_H", 48)
	c.JuryAppealH = getInt("JURY_APPEAL_H", 24)
	if firstErr != nil {
		return nil, firstErr
	}

	if !strings.HasPrefix(c.BaseURL, "http://") && !strings.HasPrefix(c.BaseURL, "https://") {
		return nil, errors.New("BASE_URL 须以 http:// 或 https:// 开头")
	}
	if c.BPGURL != "" && c.BPGKey == "" {
		return nil, errors.New("配置了 BPG_URL 就必须配置 BPG_KEY")
	}
	if c.BPGURL == "" {
		c.CertFeeEnabled = false // 没网关收不了认证费
	}
	if c.OrderTTL < 120 || c.OrderTTL > 86400 {
		return nil, errors.New("ORDER_TTL 须在 120–86400 秒之间")
	}
	if c.MinRewardE8 <= 0 || c.MaxRewardE8 < c.MinRewardE8 {
		return nil, errors.New("MIN_REWARD / MAX_REWARD 不合理")
	}
	if c.MaxSlots < 1 || c.MaxSlots > 10000 {
		return nil, errors.New("MAX_SLOTS 不合理")
	}
	if c.ClaimTTLMin < 5 || c.ClaimTTLMax < c.ClaimTTLMin || c.ClaimTTLDefault < c.ClaimTTLMin || c.ClaimTTLDefault > c.ClaimTTLMax {
		return nil, errors.New("CLAIM_TTL_* 不合理")
	}
	if !containsInt(c.RetentionOptions, c.RetentionDefault) {
		return nil, errors.New("RETENTION_DEFAULT_H 必须在 RETENTION_OPTIONS 里")
	}
	if !containsInt(c.PayWindowOptions, c.PayWindowDefault) || c.PayWindowDefault < 1 {
		return nil, errors.New("PAY_WINDOW_DEFAULT_H 必须在 PAY_WINDOW_OPTIONS 里")
	}
	if c.DeadlineDefault < 1 || c.DeadlineMax < c.DeadlineDefault {
		return nil, errors.New("DEADLINE_* 不合理")
	}
	if c.MaxVariants < 1 || c.MaxVariants > 20 || c.VerifyAttempts < 1 || c.VerifyAttempts > 20 {
		return nil, errors.New("MAX_VARIANTS / VERIFY_ATTEMPTS 不合理")
	}
	if c.JuryPanel < 3 || c.JuryPanel%2 == 0 || c.JuryInvite < c.JuryPanel || c.JuryWindowH < 1 || c.JuryAppealH < 0 {
		return nil, errors.New("JURY_* 不合理（成庭人数须为 ≥3 的奇数，邀请数 ≥ 成庭数）")
	}
	return c, nil
}

func containsInt(xs []int64, v int64) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func (c *Config) adminIPOK(ip string) bool {
	if len(c.AdminIPAllow) == 0 {
		return true
	}
	p := net.ParseIP(ip)
	if p == nil {
		return false
	}
	for _, n := range c.AdminIPAllow {
		if n.Contains(p) {
			return true
		}
	}
	return false
}
