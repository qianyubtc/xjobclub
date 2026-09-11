package main

import "strings"

// ---- 用户 ----

type User struct {
	ID              int64
	XID             string
	Handle          string
	HandleLower     string
	DisplayName     string
	AvatarURL       string
	PassHash        string
	Status          string // active | blacklisted | banned
	RegTweetID      string
	XCreatedMs      int64
	PayerID         string // 认证付款/网关付款拿到的付款方 Pay 账户 ID
	CertPaidAt      int64
	SuspendedUntil  int64 // 留存不达标暂停接单
	JuryScore       int64
	JuryTotal       int64
	JuryAgree       int64
	JuryNoShow      int64
	JuryBannedUntil int64
	HandleStale     bool
	Followers       int64 // -1 = 未知
	FollowersAt     int64
	CreatedAt       int64
	LastLoginAt     int64
}

func (u *User) Name() string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return "@" + u.Handle
}

func (u *User) XURL() string { return "https://x.com/" + u.Handle }

// Path 主页地址：用户名被顶替（HandleStale）后按 X 数字 ID 定位，避免链接指向新占用者。
func (u *User) Path() string {
	if u.HandleStale {
		return "/u/xid:" + u.XID
	}
	return "/u/" + u.Handle
}

func (u *User) Blacklisted() bool { return u.Status == "blacklisted" || u.Status == "banned" }

// PayProfile 收款设置。
type PayProfile struct {
	UserID       int64
	BinanceUID   string
	ReceiveEmail string
	Mode         string // manual | gateway
	BPGAccountID string
	APIKeyMasked string
	BPGLastOK    int64
	BPGLastErr   string
	Extra        []PayMethod // 自定义收款方式（平台不核验到账）
	UpdatedAt    int64
}

// PayMethod 用户自定义收款方式，例如「BSC 钱包地址」= 0x…。
type PayMethod struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

func (p *PayProfile) Gateway() bool { return p != nil && p.Mode == "gateway" && p.BPGAccountID != "" }

// ---- 任务 ----

type Task struct {
	ID               int64
	Code             string
	OwnerID          int64
	Title            string
	Contents         []string // 文案变体
	ContentsNorm     []string
	MatchMode        string // exact | contains
	RewardE8         int64
	Currency         string
	SlotsTotal       int64
	ClaimTTLMin      int64
	RetentionH       int64
	PayWindowH       int64
	DeadlineAt       int64
	MinAccountDays   int64
	MinFollowers     int64
	AdTag            bool
	Kind             string // post 发帖 | reply 评论 | like 点赞 | repost 转发
	TargetTweetID    string // 评论/点赞/转发的目标推文
	TargetURL        string
	TargetAuthor     string
	TargetText       string
	MinLen           int64 // 评论自由发挥时的最少字数
	DeadlineDays     int64 // 发布时填的截止天数（审核通过时重新起算）
	ReviewStartedAt  int64
	ReviewDeadlineAt int64
	ReviewResult     string // vote_pass | vote_reject | timeout_pass | admin_pass | admin_reject | skipped
	ReviewNote       string
	ReviewHold       int64  // 1 = 到期仍有反对，等管理员裁定
	PriceMode        string // fixed 固定单价 | cpm 按浏览量（reward_e8 存封顶）
	CpmE8            int64  // 每千次浏览的报酬
	FloorE8          int64  // 保底
	Status           string // open | paused | closed
	CloseReason      string
	PausedByFreeze   bool
	PublishedAt      int64
	CreatedAt        int64
	UpdatedAt        int64

	// 派生
	Used      int64 // 占用名额数
	DoneCount int64 // 已完成
	Owner     *User
}

func (t *Task) Left() int64 {
	if l := t.SlotsTotal - t.Used; l > 0 {
		return l
	}
	return 0
}

func (t *Task) Path() string { return "/t/" + t.Code }

// NeedsTweet 是否要接单方回填一条推文链接（发帖、评论）。
func (t *Task) NeedsTweet() bool { return t.Kind == "" || t.Kind == "post" || t.Kind == "reply" }

// Manual 是否由发布方人工核对（点赞、转发：公开接口读不到名单）。
func (t *Task) Manual() bool { return t.Kind == "like" || t.Kind == "repost" }

func (t *Task) KindText() string { return kindText(t.Kind) }

// CPM 是否按浏览量计价（reward_e8 是封顶，结算金额写在记录上）。
func (t *Task) CPM() bool { return t.PriceMode == "cpm" }

// PriceText 计价说明。
func (t *Task) PriceText() string {
	if t.CPM() {
		return fmtE8(t.CpmE8) + " U / 千浏览 · 保底 " + fmtE8(t.FloorE8) + " · 封顶 " + fmtE8(t.RewardE8) + " U"
	}
	return fmtE8(t.RewardE8) + " U / " + t.Unit()
}

// payAmount 一条记录应付多少：按浏览量结算过就用结算金额，否则用任务单价（封顶）。
func payAmount(x *Submission, t *Task) int64 {
	if x != nil && x.SettleViews >= 0 { // 已按浏览量结算（金额可能等于保底）
		return x.AmountE8
	}
	return t.RewardE8
}

// cpmAmount 浏览量 → 报酬：views/1000 × 单价，夹在保底与封顶之间，取到 4 位小数。
func cpmAmount(t *Task, views int64) int64 {
	if views < 0 {
		views = 0
	}
	amt := views * t.CpmE8 / 1000
	if amt < t.FloorE8 {
		amt = t.FloorE8
	}
	if amt > t.RewardE8 {
		amt = t.RewardE8
	}
	return amt / 10000 * 10000
}

// Unit 计价单位。
func (t *Task) Unit() string {
	if t.Manual() {
		return "次"
	}
	return "条"
}

// DoneVerb 接单方要做的动作。
func (t *Task) DoneVerb() string {
	switch t.Kind {
	case "reply":
		return "评论"
	case "like":
		return "点赞"
	case "repost":
		return "转发"
	}
	return "发帖"
}

func kindText(k string) string {
	switch k {
	case "reply":
		return "评论"
	case "like":
		return "点赞"
	case "repost":
		return "转发"
	}
	return "发帖"
}

// 接单记录状态
const (
	SClaimed  = "claimed"
	SSubmit   = "submitted"
	SChecking = "checking" // 点赞/转发：接单方已声明完成，等发布方核对
	SVerified = "verified"
	SPayable  = "payable"
	SAwait    = "awaiting_confirm"
	SPaid     = "paid"
	SOverdue  = "overdue"
	SDisputed = "disputed"
	SVoid     = "void"
	SExpired  = "expired"
	SDefault  = "defaulted"
)

var subStatusText = map[string]string{
	SClaimed: "已接单", SSubmit: "验证中", SChecking: "待核对", SVerified: "已验证", SPayable: "待付款", SAwait: "待确认到账",
	SPaid: "已完成", SOverdue: "逾期未付", SDisputed: "申诉中", SVoid: "作废", SExpired: "未按时提交", SDefault: "违约",
}

func subStatus(s string) string {
	if v, ok := subStatusText[s]; ok {
		return v
	}
	return s
}

// terminalSub 是否终态。
func terminalSub(s string) bool {
	return s == SPaid || s == SVoid || s == SExpired || s == SDefault
}

// slotFree 该状态的记录是否不占名额（过期、作废释放名额）。
func slotFree(s string) bool { return s == SExpired || s == SVoid }

type Submission struct {
	ID             int64
	Code           string
	TaskID         int64
	WorkerID       int64
	VariantIdx     int64
	Status         string
	PrevStatus     string // 进入 disputed 前的状态
	ClaimedAt      int64
	ClaimExpiresAt int64
	TweetID        string
	TweetURL       string
	TweetRoot      string // 推文编辑组的原始 ID（同一条推文的各编辑版本共用，防一帖多投）
	TweetText      string
	TweetCreatedAt int64
	VerifyAttempts int64
	VerifyRetries  int64
	LastError      string
	NextVerifyAt   int64
	VerifiedAt     int64
	RecheckDueAt   int64
	RecheckFlag    string
	RecheckTries   int64
	PayableAt      int64
	PayDeadlineAt  int64
	OverdueAt      int64
	ReportedAt     int64
	GraceUntil     int64
	MarkedPaidAt   int64
	MarkedOrderID  string
	MarkedNote     string
	UnderpaidE8    int64 // 网关少付：实付金额（0 = 无少付）
	TopupRequested int64 // 接单方要求补差的时间（期间不自动完成）
	TopupMarkedAt  int64 // 发布方登记补差订单号的时间
	TopupOrderID   string
	ConfirmedAt    int64
	ConfirmMethod  string
	PaidAmountE8   int64
	Late           bool
	VoidReason     string
	DefaultedAt    int64
	SelfDeal       int64 // 疑似自导自演（不计信用）
	Unreadable     int64 // 复检连续读不到次数
	CheckingAt     int64 // 点赞/转发：提交核对的时间
	CheckNote      string
	CheckRejects   int64
	CheckAuto      int64 // 1 = 发布方超时未核对，视为通过
	AmountE8       int64 // 按浏览量结算后的应付金额（0 = 用任务单价）
	Views          int64 // 记录到的最大浏览量，-1 未知
	ViewsAt        int64
	SettleViews    int64 // 结算时采用的浏览量，-1 未结算
	CreatedAt      int64
	UpdatedAt      int64

	Task   *Task
	Worker *User
}

func (s *Submission) Path() string { return "/s/" + s.Code }

// ---- 付款单 ----

type Payment struct {
	ID              int64
	SubmissionID    int64
	Kind            string // gateway | manual | topup | cert
	UserID          int64  // 付款方
	BPGOrderID      string
	MerchantOrderID string
	PayAmount       string
	BaseE8          int64
	NoteCode        string
	PayURL          string
	ReceiveUID      string
	ReceiveLink     string
	Status          string // pending | paid | underpaid | expired | closed
	ActualE8        int64
	MatchedBy       string
	BinanceOrderID  string
	PayerID         string
	PaidAt          int64
	ExpiresAt       int64
	RawCallback     string
	LastSyncAt      int64
	CreatedAt       int64
	UpdatedAt       int64
}

// ---- 申诉 ----

var disputeTypeText = map[string]string{
	"A": "未付款", "B": "标记已付但未收到", "C": "验证误判", "D": "未完成或不合格", "E": "任务违规", "F": "黑名单申诉", "G": "核对争议",
}

func disputeType(t string) string {
	if v, ok := disputeTypeText[t]; ok {
		return v
	}
	return t
}

var disputeStatusText = map[string]string{
	"open": "已发起", "evidence": "举证中", "review": "管理员审理中", "jury": "小法庭投票中", "appeal": "上诉期", "resolved": "已结案",
}

func disputeStatus(s string) string {
	if v, ok := disputeStatusText[s]; ok {
		return v
	}
	return s
}

type Dispute struct {
	ID             int64
	Code           string
	Type           string
	SubmissionID   int64
	TaskID         int64
	OpenerID       int64
	AgainstID      int64
	Status         string
	EvidenceUntil  int64
	Resolution     string // upheld | rejected | partial
	ResolutionNote string
	ResolvedBy     int64 // 0 = 小法庭
	ResolvedAt     int64
	JuryCaseID     int64
	AppealUntil    int64
	AppealBy       int64
	CreatedAt      int64
	UpdatedAt      int64

	Opener  *User
	Against *User
	Sub     *Submission
	Task    *Task
}

func (d *Dispute) Path() string { return "/d/" + d.Code }

type DisputeMessage struct {
	ID        int64
	DisputeID int64
	AuthorID  int64 // 0 = 系统 / 管理员
	Text      string
	Images    []string
	CreatedAt int64
	Author    *User
}

// ---- 黑名单 ----

type BlacklistEntry struct {
	ID           int64
	UserID       int64
	XID          string
	Handle       string
	BinanceUID   string
	PayerID      string
	Role         string // publisher | worker
	Reason       string
	DisputeID    int64
	DisputeCode  string
	AmountOwedE8 int64
	RepaidAt     int64
	LiftedAt     int64
	LiftedBy     int64
	Note         string
	CreatedAt    int64
}

// ---- 小法庭 ----

type JuryCase struct {
	ID           int64
	DisputeID    int64
	Status       string // voting | closed | escalated
	Round        int64
	DeadlineAt   int64
	Verdict      string // for | against | none
	VotesFor     int64
	VotesAgainst int64
	Abstain      int64
	CreatedAt    int64
	ClosedAt     int64
}

type JuryVote struct {
	CaseID    int64
	JurorID   int64
	Vote      string
	Reason    string
	CreatedAt int64
}

// ---- 通知 ----

type Notification struct {
	ID        int64
	UserID    int64
	Kind      string
	Title     string
	Body      string
	Link      string
	ReadAt    int64
	CreatedAt int64
}

// ---- 信用等级 ----

type Tier struct {
	Name       string
	ExposureE8 int64
	OpenTasks  int64
	Concurrent int64
	Daily      int64
}

// PubStats 发布方公开统计。
type PubStats struct {
	Paid         int64
	PaidCredit   int64 // 计信用的付款：网关核销 / 接单方确认 / 裁决完成，且非自导自演（不含超时自动完成）；升级只数这个
	PaidGateway  int64 // 其中网关自动核销的
	PaidDistinct int64 // 计信用付款来自多少个不同的接单方（资深门槛，防两个号互刷）
	Overdue      int64 // 逾期付款（含已补付）
	Defaulted    int64
	AvgPayMs     int64
	OpenTasks    int64
	ExposureE8   int64
	Frozen       bool
	CheckVoids   int64 // 点赞/转发：两次核对未见到而作废的记录数（公示，防白嫖）
}

// WorkerStats 接单方公开统计。
type WorkerStats struct {
	Done         int64
	DoneCredit   int64 // 计信用的完成：同 PaidCredit 口径
	DoneGateway  int64 // 其中网关自动核销的
	DoneDistinct int64 // 计信用完成来自多少个不同的发布方
	Void30d      int64
	Active       int64 // 进行中的接单
	Today        int64
	EarnedE8     int64
	Suspended    bool
	AwaitCount   int64
}

func joinLower(xs []string) string { return strings.ToLower(strings.Join(xs, "\n")) }

// auditTextMap 记录页时间线显示的动作文案；空字符串表示不显示（sub.claim 已有合成的「接单」行）。
var auditTextMap = map[string]string{
	"sub.claim": "", "sub.submit": "提交链接，开始验证", "sub.verified": "验证通过", "sub.force_verified": "管理员判定验证通过", "sub.payable": "留存复检通过，进入待付款",
	"sub.expired": "超时未提交，名额释放", "sub.void": "作废", "sub.overdue": "付款逾期", "sub.mark_paid": "发布方登记已付", "sub.paid": "付款完成", "sub.repaid": "补付到账",
	"sub.underpaid": "少付处理", "sub.topup_requested": "接单方要求补差", "sub.defaulted": "记为违约", "sub.checking": "提交核对", "sub.paynow": "发布方选择提前付款，不等留存到期", "sub.auto_confirm": "待确认到账超时未处理，视为已收到自动完成", "sub.selfdeal": "管理员调整自导自演标记", "sub.check_ok": "发布方确认已完成",
	"sub.check_no": "发布方未见到，退回重做", "sub.check_void": "两次核对未见到，作废", "sub.check_auto": "发布方超时未核对，视为通过", "sub.repost_detected": "自动检测到转发",
	"dispute.open": "发起申诉", "dispute.resolve": "申诉裁决", "dispute.auto_close": "申诉自动结案", "task.takedown": "任务被下架",
	"pay.payer_mismatch": "付款账户与认证不一致", "pay.unexpected": "收到未预期的付款",
}

func auditText(action string) string {
	if v, ok := auditTextMap[action]; ok {
		return v
	}
	return action
}

// TaskVote 发布审核的一票。
type TaskVote struct {
	UserID    int64
	Vote      int64 // 1 通过 / -1 反对
	Reason    string
	CreatedAt int64
}
