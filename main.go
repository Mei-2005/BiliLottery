package main

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/skip2/go-qrcode"
)

const (
	biliAPIBase      = "https://api.bilibili.com"
	biliPassport     = "https://passport.bilibili.com"
	biliLiveAPI      = "https://api.live.bilibili.com"
	userAgent        = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36"
	commentPageSize  = 20
	commentPageDelay = 500 * time.Millisecond
	medalDelay       = 300 * time.Millisecond
	// maxCommentPages 是翻页硬上限，仅用于接口异常时的兜底
	maxCommentPages = 200
)

//go:embed web/index.html
var webFS embed.FS

type App struct {
	server *http.Server

	mu       sync.RWMutex
	sessdata string
	busy     bool

	logMu sync.Mutex
	logs  []string

	qrMu   sync.RWMutex
	qrFile string

	avatarMu    sync.Mutex
	avatarCache map[string]avatarCacheEntry

	resultMu sync.RWMutex
	result   *LotteryResult

	progMu sync.RWMutex
	prog   *lotteryProgress

	wbiMu sync.Mutex
	wbi   *wbiKeys

	shutdownOnce sync.Once
	shutdownCh   chan struct{}

	// 页面最后一次请求的时间：用来区分“关闭标签页”和“刷新”
	reqMu   sync.Mutex
	lastReq time.Time
	// 是否收到过任何页面请求：用来判断浏览器到底有没有打开
	reqSeen bool
}

type FlexibleInt64 int64

func (v *FlexibleInt64) UnmarshalJSON(data []byte) error {
	s := strings.TrimSpace(string(data))
	if s == "" || s == "null" {
		*v = 0
		return nil
	}
	if s[0] != '"' {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return err
		}
		*v = FlexibleInt64(n)
		return nil
	}
	var str string
	if err := json.Unmarshal(data, &str); err != nil {
		return err
	}
	if str == "" {
		*v = 0
		return nil
	}
	n, err := strconv.ParseInt(str, 10, 64)
	if err != nil {
		return err
	}
	*v = FlexibleInt64(n)
	return nil
}

type VideoInfo struct {
	AID      int64
	BVID     string
	Title    string
	UPName   string
	UPUID    int64
	UPAvatar string
}

type videoResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		AID   int64  `json:"aid"`
		BVID  string `json:"bvid"`
		Title string `json:"title"`
		Owner struct {
			Mid  FlexibleInt64 `json:"mid"`
			Name string        `json:"name"`
			Face string        `json:"face"`
		} `json:"owner"`
	} `json:"data"`
}

// fansDetail 是评论接口里 member.fans_detail：这条评论佩戴的粉丝团灯牌，
// 也就是评论区里显示的那个灯牌（该视频 UP 的粉丝团灯牌）。
// 该标签对拥有该粉丝团灯牌的人都有效——无论是否佩戴、是否隐藏，
// 所以核对资格直接用它判定，且它随评论一起返回、不需要额外请求。
type fansDetail struct {
	MedalID   FlexibleInt64 `json:"medal_id"`
	MedalName string        `json:"medal_name"`
	Level     FlexibleInt64 `json:"level"`
	Intimacy  FlexibleInt64 `json:"intimacy"`
}

type Comment struct {
	RPID int64 `json:"rpid"`
	// Root 是二级评论所属一级评论的 RPID（一级评论为 0）
	Root FlexibleInt64 `json:"root"`
	// RCount 是这一楼的回复数：为 0 说明没有二级评论，不必去请求
	RCount int `json:"rcount"`
	Member struct {
		Mid       FlexibleInt64 `json:"mid"`
		Name      string        `json:"uname"`
		Face      string        `json:"avatar"`
		FaceAlt   string        `json:"face"`
		LevelInfo struct {
			CurrentLevel int `json:"current_level"`
		} `json:"level_info"`
		FansDetail *fansDetail `json:"fans_detail"`
	} `json:"member"`
	Content struct {
		Message string `json:"message"`
	} `json:"content"`
}

// commentCursor 是新版评论区的游标信息：页码翻页失效时靠它继续往后取
type commentCursor struct {
	IsEnd    bool `json:"is_end"`
	AllCount int  `json:"all_count"`
	// PaginationStr 有时直接以 JSON 字符串形式给出 {"offset":"..."}
	PaginationStr string `json:"pagination_str"`
	// PaginationReply.NextOffset 用于下一次游标翻页
	PaginationReply struct {
		NextOffset string `json:"next_offset"`
	} `json:"pagination_reply"`
}

type commentResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Page struct {
			Num   int `json:"num"`
			Size  int `json:"size"`
			Count int `json:"count"`
		} `json:"page"`
		Cursor  commentCursor `json:"cursor"`
		Replies []Comment     `json:"replies"`
		// TopReplies 是置顶评论，新版接口常常只把它放在这里而不放进 replies
		TopReplies []Comment `json:"top_replies"`
		Upper      struct {
			Top *Comment `json:"top"`
		} `json:"upper"`
	} `json:"data"`
}

type MedalInfo struct {
	TargetID      FlexibleInt64 `json:"target_id"`
	Level         int           `json:"level"`
	MedalName     string        `json:"medal_name"`
	WearingStatus int           `json:"wearing_status"`
	MedalID       int64         `json:"medal_id"`
}

type MedalItem struct {
	MedalInfo  MedalInfo `json:"medal_info"`
	TargetName string    `json:"target_name"`
}

type medalResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		List []MedalItem `json:"list"`
	} `json:"data"`
}

type qrGenerateResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		URL       string `json:"url"`
		QRCodeKey string `json:"qrcode_key"`
	} `json:"data"`
}

type qrPollResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		URL     string `json:"url"`
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"data"`
}

type LotteryCandidate struct {
	UID        int64  `json:"uid"`
	Name       string `json:"name"`
	Content    string `json:"content"`
	Level      int    `json:"level"`
	MedalID    int64  `json:"medalId"`
	MedalName  string `json:"medalName"`
	MedalLevel int    `json:"medalLevel"`
	AvatarURL  string `json:"avatar"`
}

type lotteryVideo struct {
	BV       string `json:"bv"`
	Title    string `json:"title"`
	UPName   string `json:"upName"`
	UPUID    int64  `json:"upUid"`
	UPAvatar string `json:"upAvatar"`
}

type LotteryResult struct {
	Done       bool               `json:"done"`
	Video      *lotteryVideo      `json:"video,omitempty"`
	Candidates []LotteryCandidate `json:"candidates"`
	Winners    []LotteryCandidate `json:"winners"`
}

type LotteryConfig struct {
	RequireLevel  bool
	MinLevel      int
	RequireMedal  bool
	MinMedalLevel int
	// ExcludeHiddenList 决定用哪种方式判定粉丝团灯牌：
	// false（默认）→ 用"评论区的粉丝团灯牌"（评论接口的 member.fans_detail），最全；
	// true → 只看"用户个人的粉丝团灯牌列表"（勋章墙 MedalWall）：
	//        隐藏了粉丝团灯牌、或选了"仅显示当前佩戴"却没佩戴上当前 UP 粉丝团灯牌的人，都会被排除。
	ExcludeHiddenList  bool
	IncludeSubComments bool
	DrawCount          int
}

// ---------------------------------------------------------------------------
// 抓取 / 筛选 实时进度（供前端做可视化，不参与抽奖逻辑）
// ---------------------------------------------------------------------------

const (
	stageVideo    = "video"
	stageComments = "comments"
	stageFilter   = "filter"
	stageDraw     = "draw"
	stageDone     = "done"
	stageFailed   = "failed"
)

const (
	// progressRecentMax 是下发给前端的“最近检查用户”数量上限
	progressRecentMax = 24
	// progressPoolMax 是下发给前端的候选池预览数量上限
	progressPoolMax = 160
	// progressCommentMax 是下发给前端的“最近评论”数量上限（滚动弹幕流）；
	// 弹幕按“一楼 + 其全部回复”成组下发，窗口开大一些避免把一组从中间截断
	progressCommentMax = 600
	// progressCommentRunes 是弹幕里评论内容的截断长度
	progressCommentRunes = 800
)

// progressUser 描述单个被检查用户在可视化里的展示信息
type progressUser struct {
	UID        int64  `json:"uid"`
	Name       string `json:"name"`
	Avatar     string `json:"avatar"`
	Level      int    `json:"level"`
	MedalName  string `json:"medalName,omitempty"`
	MedalLevel int    `json:"medalLevel,omitempty"`
	Passed     bool   `json:"passed"`
	Reason     string `json:"reason,omitempty"`
	Seq        int    `json:"seq"`
}

// progressComment 是下发给前端的“刚抓到的评论”，用于滚动弹幕展示
type progressComment struct {
	RPID    int64  `json:"rpid"`
	Root    int64  `json:"root,omitempty"` // 二级评论所属一级评论的 rpid，供前端挂到对应弹幕下
	UID     int64  `json:"uid"`
	Name    string `json:"name"`
	Avatar  string `json:"avatar"`
	Content string `json:"content"`
	Sub     bool   `json:"sub,omitempty"`
}

type lotteryProgress struct {
	Running   bool   `json:"running"`
	Stage     string `json:"stage"`
	StageText string `json:"stageText"`
	StartedAt int64  `json:"startedAt"`
	ElapsedMS int64  `json:"elapsedMs"`
	Error     string `json:"error,omitempty"`

	BV         string `json:"bv,omitempty"`
	VideoTitle string `json:"videoTitle,omitempty"`
	UPName     string `json:"upName,omitempty"`
	UPAvatar   string `json:"upAvatar,omitempty"`
	UPUID      int64  `json:"upUid,omitempty"`

	PagesFetched    int `json:"pagesFetched"`
	PagesTotal      int `json:"pagesTotal"`
	CommentsFetched int `json:"commentsFetched"`
	CommentsTotal   int `json:"commentsTotal"`
	SubFetched      int `json:"subFetched"`

	UsersTotal   int `json:"usersTotal"`
	UsersChecked int `json:"usersChecked"`
	UsersPassed  int `json:"usersPassed"`
	UsersFailed  int `json:"usersFailed"`

	DrawCount  int `json:"drawCount"`
	Candidates int `json:"candidates"`

	Recent []progressUser `json:"recent"`
	Pool   []progressUser `json:"pool"`

	Comments []progressComment `json:"comments"`
}

type lotteryRequest struct {
	BV                 string `json:"bv"`
	RequireLevel       bool   `json:"requireLevel"`
	MinLevel           int    `json:"minLevel"`
	RequireMedal       bool   `json:"requireMedal"`
	MinMedalLevel      int    `json:"minMedalLevel"`
	ExcludeHiddenList  bool   `json:"excludeHiddenList"`
	IncludeSubComments bool   `json:"includeSubComments"`
	DrawCount          int    `json:"drawCount"`
}

type apiResponse struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

type statusResponse struct {
	LoggedIn bool     `json:"loggedIn"`
	Logs     []string `json:"logs"`
	Busy     bool     `json:"busy"`
}

func main() {
	app := &App{shutdownCh: make(chan struct{})}

	mux := http.NewServeMux()
	mux.HandleFunc("/", app.handleIndex)
	mux.HandleFunc("/api/status", app.handleStatus)
	mux.HandleFunc("/api/login/start", app.handleLoginStart)
	mux.HandleFunc("/api/login/qr", app.handleQR)
	mux.HandleFunc("/api/lottery", app.handleLotteryHTTP)
	mux.HandleFunc("/api/result", app.handleResult)
	mux.HandleFunc("/api/progress", app.handleProgress)
	mux.HandleFunc("/api/avatar", app.handleAvatar)
	mux.HandleFunc("/api/shutdown", app.handleShutdown)
	mux.HandleFunc("/api/ui-closed", app.handleUIClosed)

	app.server = &http.Server{
		// 包一层：记录页面最后一次请求时间，用于判断标签页是否真的关了
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			app.noteRequest()
			mux.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		showFatalError(err)
		return
	}

	port := ln.Addr().(*net.TCPAddr).Port
	browserURL := fmt.Sprintf("http://127.0.0.1:%d/", port)

	app.appendLog("本地服务已启动：%s", browserURL)

	go func() {
		if err := app.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			app.appendLog("HTTP服务异常：%v", err)
		}
	}()

	if err := openBrowser(browserURL); err != nil {
		app.appendLog("无法自动打开浏览器：%v", err)
		app.appendLog("请手动打开：%s", browserURL)
	}

	// 浏览器没打开时不能什么都不发生：等一会儿还没收到任何页面请求，
	// 就把地址弹给用户。本程序没有控制台窗口，系统弹窗是唯一能被看见的提示；
	// Windows 的“默认浏览器关联”损坏时，start/rundll32 会静默失败、连错误都不返回。
	go func() {
		select {
		case <-app.shutdownCh:
			return
		case <-time.After(10 * time.Second):
		}

		if app.pageSeen() {
			return
		}

		app.appendLog("没有检测到页面请求，浏览器可能没有自动打开；请手动访问：%s", browserURL)
		showNotice("BiliLottery 已在运行",
			"浏览器没有自动打开。\n\n请在浏览器里手动打开下面的地址：\n\n"+browserURL)
	}()

	<-app.shutdownCh
	app.cleanup()

	// 给在途请求一点时间收尾；若有连接一直占着，就强制关闭，避免进程卡住不退出
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := app.server.Shutdown(ctx); err != nil {
		_ = app.server.Close()
	}
}

func (app *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func (app *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	app.mu.RLock()
	busy, logged := app.busy, app.sessdata != ""
	app.mu.RUnlock()

	app.logMu.Lock()
	logs := append([]string(nil), app.logs...)
	app.logMu.Unlock()

	writeJSON(w, statusResponse{LoggedIn: logged, Busy: busy, Logs: logs})
}

func (app *App) handleResult(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	res := app.getResult()
	if res == nil {
		writeJSON(w, LotteryResult{Candidates: []LotteryCandidate{}, Winners: []LotteryCandidate{}})
		return
	}
	writeJSON(w, *res)
}

func (app *App) getResult() *LotteryResult {
	app.resultMu.RLock()
	defer app.resultMu.RUnlock()
	if app.result == nil {
		return nil
	}
	c := *app.result
	return &c
}

func (app *App) setResult(res *LotteryResult) {
	app.resultMu.Lock()
	app.result = res
	app.resultMu.Unlock()
}

func (app *App) clearResult() {
	app.resultMu.Lock()
	app.result = nil
	app.resultMu.Unlock()
}

// ---------------------------------------------------------------------------
// 进度：写入
// ---------------------------------------------------------------------------

func (app *App) setProgress(fn func(p *lotteryProgress)) {
	app.progMu.Lock()
	defer app.progMu.Unlock()
	if app.prog == nil {
		app.prog = &lotteryProgress{}
	}
	fn(app.prog)
}

// startProgress 开启一轮新的进度（会清空上一轮的统计）
func (app *App) startProgress() {
	now := time.Now().UnixMilli()
	app.setProgress(func(p *lotteryProgress) {
		*p = lotteryProgress{
			Running:   true,
			Stage:     stageVideo,
			StageText: "正在解析视频信息",
			StartedAt: now,
		}
	})
}

func (app *App) finishProgress(stage, text string) {
	app.setProgress(func(p *lotteryProgress) {
		p.Running = false
		p.Stage = stage
		p.StageText = text
		p.ElapsedMS = elapsedSince(p.StartedAt)
	})
}

// failProgress 结束当前进度并标记为失败
func (app *App) failProgress(format string, args ...interface{}) {
	app.setProgress(func(p *lotteryProgress) {
		p.Running = false
		p.Stage = stageFailed
		p.StageText = "任务中断"
		p.Error = fmt.Sprintf(format, args...)
		p.ElapsedMS = elapsedSince(p.StartedAt)
	})
}

// reportProgressUser 上报一个刚检查完的用户（用于筛选阶段的可视化）
func (app *App) reportProgressUser(u progressUser) {
	app.setProgress(func(p *lotteryProgress) {
		p.UsersChecked = u.Seq
		if u.Passed {
			p.UsersPassed++
			p.Candidates = p.UsersPassed
			if len(p.Pool) < progressPoolMax {
				p.Pool = append(p.Pool, u)
			}
		} else {
			p.UsersFailed++
		}

		p.Recent = append(p.Recent, u)
		if len(p.Recent) > progressRecentMax {
			p.Recent = append([]progressUser(nil), p.Recent[len(p.Recent)-progressRecentMax:]...)
		}
	})
}

// previewAvatar 只做本地归一化，不额外请求 B 站接口，避免拖慢筛选速度
func previewAvatar(raw string) string {
	return normalizeAvatarURL(raw)
}

// makeProgressComment 把一条评论转成下发给前端的弹幕数据
func makeProgressComment(c Comment, sub bool, rootID int64) progressComment {
	content := c.Content.Message
	runes := []rune(content)
	if len(runes) > progressCommentRunes {
		content = string(runes[:progressCommentRunes]) + "…"
	}

	if !sub {
		rootID = 0
	}

	return progressComment{
		RPID:    c.RPID,
		Root:    rootID,
		UID:     int64(c.Member.Mid),
		Name:    c.Member.Name,
		Avatar:  previewAvatar(normalizeAvatarURL(firstNonEmpty(c.Member.Face, c.Member.FaceAlt))),
		Content: content,
		Sub:     sub,
	}
}

// reportProgressCommentsGrouped 按“一级 + 它的全部二级回复”成组推送给前端：
// 同一楼的所有回复必须全部到齐后才作为一个整体显示，
// 不会出现零散的单条二级评论
func (app *App) reportProgressCommentsGrouped(roots []Comment, subsByRoot map[int64][]Comment) {
	if len(roots) == 0 {
		return
	}

	app.setProgress(func(p *lotteryProgress) {
		for _, root := range roots {
			p.Comments = append(p.Comments, makeProgressComment(root, false, 0))
			for _, sub := range subsByRoot[root.RPID] {
				p.Comments = append(p.Comments, makeProgressComment(sub, true, root.RPID))
			}
		}

		if len(p.Comments) > progressCommentMax {
			p.Comments = append([]progressComment(nil), p.Comments[len(p.Comments)-progressCommentMax:]...)
		}
	})
}

func elapsedSince(startedAtMS int64) int64 {
	if startedAtMS <= 0 {
		return 0
	}
	d := time.Since(time.UnixMilli(startedAtMS))
	if d < 0 {
		return 0
	}
	return d.Milliseconds()
}

// ---------------------------------------------------------------------------
// 进度：读取
// ---------------------------------------------------------------------------

func (app *App) getProgress() lotteryProgress {
	app.progMu.RLock()
	defer app.progMu.RUnlock()

	if app.prog == nil {
		return lotteryProgress{
			Stage:    "idle",
			Recent:   []progressUser{},
			Pool:     []progressUser{},
			Comments: []progressComment{},
		}
	}

	c := *app.prog
	c.Recent = append([]progressUser{}, app.prog.Recent...)
	c.Pool = append([]progressUser{}, app.prog.Pool...)
	c.Comments = append([]progressComment{}, app.prog.Comments...)
	if c.Running {
		c.ElapsedMS = elapsedSince(c.StartedAt)
	}
	return c
}

func (app *App) handleProgress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, app.getProgress())
}

func (app *App) handleQR(w http.ResponseWriter, r *http.Request) {
	app.qrMu.RLock()
	file := app.qrFile
	app.qrMu.RUnlock()
	if file == "" {
		http.NotFound(w, r)
		return
	}

	data, err := os.ReadFile(file)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func (app *App) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	app.mu.Lock()
	if app.busy {
		app.mu.Unlock()
		writeJSON(w, apiResponse{OK: false, Message: "当前已有任务正在运行"})
		return
	}
	app.busy = true
	app.mu.Unlock()

	writeJSON(w, apiResponse{OK: true, Message: "二维码生成中"})

	go func() {
		defer func() {
			app.mu.Lock()
			app.busy = false
			app.mu.Unlock()
		}()

		app.deleteQRFile()
		app.appendLog("开始 Bilibili 扫码登录")

		qrKey, qrURL, err := app.generateQRCode()
		if err != nil {
			app.appendLog("生成二维码失败：%v", err)
			return
		}

		file, err := app.createQRImage(qrURL)
		if err != nil {
			app.appendLog("生成二维码图片失败：%v", err)
			return
		}

		app.qrMu.Lock()
		app.qrFile = file
		app.qrMu.Unlock()
		app.appendLog("二维码已生成，请使用 Bilibili APP 扫码")

		sessdata, err := app.pollQRCode(qrKey)
		if err != nil {
			app.appendLog("扫码登录失败：%v", err)
			app.deleteQRFile()
			return
		}

		app.setSessdata(sessdata)
		app.appendLog("扫码登录成功")
		app.deleteQRFile()
	}()
}

func (app *App) requestShutdown() {
	app.shutdownOnce.Do(func() { close(app.shutdownCh) })
}

func (app *App) noteRequest() {
	app.reqMu.Lock()
	app.lastReq = time.Now()
	app.reqSeen = true
	app.reqMu.Unlock()
}

// pageSeen 表示至少有一个页面请求进来过，也就是说浏览器确实把页面打开了
func (app *App) pageSeen() bool {
	app.reqMu.Lock()
	defer app.reqMu.Unlock()
	return app.reqSeen
}

func (app *App) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	writeJSON(w, apiResponse{OK: true, Message: "程序正在退出"})
	app.requestShutdown()
}

// uiGoneGrace 是“页面关闭通知”后的等待时间：这段时间内等不到新的页面请求，
// 就认为标签页真的被关了，退出程序；刷新会在几秒内重新发起轮询，从而取消退出。
const uiGoneGrace = 4 * time.Second

func (app *App) handleUIClosed(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	app.reqMu.Lock()
	anchor := app.lastReq
	app.reqMu.Unlock()

	writeJSON(w, apiResponse{OK: true})

	go func() {
		time.Sleep(uiGoneGrace)

		app.reqMu.Lock()
		last := app.lastReq
		app.reqMu.Unlock()

		if last.Equal(anchor) {
			app.requestShutdown()
		}
	}()
}

func (app *App) handleLotteryHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req lotteryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, apiResponse{OK: false, Message: "请求参数错误"})
		return
	}

	// 先做可以同步判定的校验：否则任务注定失败，界面却先收到“已开始”
	if bv := strings.TrimSpace(req.BV); bv == "" || !strings.HasPrefix(strings.ToUpper(bv), "BV") {
		writeJSON(w, apiResponse{OK: false, Message: "请输入正确的 BV 号"})
		return
	}
	if app.getSessdata() == "" {
		writeJSON(w, apiResponse{OK: false, Message: "请先扫码登录 Bilibili"})
		return
	}
	if _, err := validateConfig(req); err != nil {
		writeJSON(w, apiResponse{OK: false, Message: err.Error()})
		return
	}

	app.mu.Lock()
	if app.busy {
		app.mu.Unlock()
		writeJSON(w, apiResponse{OK: false, Message: "当前已有任务正在运行"})
		return
	}
	app.busy = true
	app.mu.Unlock()

	writeJSON(w, apiResponse{OK: true, Message: "抽奖任务已开始，请查看运行日志"})

	go func() {
		defer func() {
			app.mu.Lock()
			app.busy = false
			app.mu.Unlock()
		}()
		app.runLottery(req)
	}()
}

func (app *App) runLottery(req lotteryRequest) {
	// 进度从这里就开始记：即使开局校验失败，前端也能拿到明确的结束状态，
	// 不会一直停在“抽奖进行中”
	app.startProgress()

	bv := strings.TrimSpace(req.BV)
	if bv == "" || !strings.HasPrefix(strings.ToUpper(bv), "BV") {
		app.appendLog("错误：请输入正确的 BV 号")
		app.failProgress("请输入正确的 BV 号")
		return
	}

	if app.getSessdata() == "" {
		app.appendLog("错误：请先扫码登录 Bilibili")
		app.failProgress("请先扫码登录 Bilibili")
		return
	}

	config, err := validateConfig(req)
	if err != nil {
		app.appendLog("配置错误：%v", err)
		app.failProgress("配置错误：%v", err)
		return
	}

	app.appendLog("")
	app.appendLog("========================================")
	app.appendLog("开始抽奖")
	app.appendLog("========================================")

	app.clearResult()

	video, err := app.getVideoInfo(bv)
	if err != nil {
		app.appendLog("获取视频信息失败：%v", err)
		app.failProgress("获取视频信息失败：%v", err)
		return
	}

	app.appendLog("视频：%s", video.Title)
	app.appendLog("UP主：%s (UID=%d)", video.UPName, video.UPUID)

	app.setProgress(func(p *lotteryProgress) {
		p.Stage = stageComments
		p.StageText = "正在收集评论"
		p.BV = video.BVID
		p.VideoTitle = video.Title
		p.UPName = video.UPName
		p.UPUID = video.UPUID
		p.UPAvatar = video.UPAvatar
	})

	comments, err := app.getComments(video.AID, config.IncludeSubComments, func(fetched, total, page, pages int) {
		app.setProgress(func(p *lotteryProgress) {
			p.CommentsFetched = fetched
			p.CommentsTotal = total
			p.PagesFetched = page
			p.PagesTotal = pages
		})
	})
	if err != nil {
		app.appendLog("获取评论失败：%v", err)
		app.failProgress("获取评论失败：%v", err)
		return
	}

	// 抓取结束，以实际条数收尾，保证面板数字与日志“共 N 条评论”一致
	app.setProgress(func(p *lotteryProgress) {
		p.CommentsFetched = len(comments)
		if p.CommentsTotal < len(comments) {
			p.CommentsTotal = len(comments)
		}
	})

	if len(comments) == 0 {
		app.appendLog("没有获取到符合设置的评论")
		app.failProgress("没有获取到任何评论")
		return
	}

	type UserComment struct {
		UID       int64
		Name      string
		Content   string
		Level     int
		AvatarURL string
		// Medal 是评论自带的粉丝团灯牌标签（member.fans_detail），
		// 也就是评论区里显示的那个灯牌；MedalName 为空表示这条评论没有带牌
		Medal MedalInfo
	}

	medalOfComment := func(c Comment) MedalInfo {
		fd := c.Member.FansDetail
		if fd == nil || fd.MedalName == "" {
			return MedalInfo{}
		}
		return MedalInfo{
			TargetID:  FlexibleInt64(video.UPUID),
			MedalID:   int64(fd.MedalID),
			MedalName: fd.MedalName,
			Level:     int(fd.Level),
		}
	}

	unique := make(map[int64]UserComment)
	for _, c := range comments {
		uid := int64(c.Member.Mid)
		if uid <= 0 {
			continue
		}
		if prev, ok := unique[uid]; ok {
			// 同一用户可能有多条评论：只要其中一条带了粉丝团灯牌就补上
			if prev.Medal.MedalName == "" {
				prev.Medal = medalOfComment(c)
				unique[uid] = prev
			}
			continue
		}
		unique[uid] = UserComment{
			UID:       uid,
			Name:      c.Member.Name,
			Content:   c.Content.Message,
			Level:     c.Member.LevelInfo.CurrentLevel,
			AvatarURL: normalizeAvatarURL(firstNonEmpty(c.Member.Face, c.Member.FaceAlt)),
			Medal:     medalOfComment(c),
		}
	}

	app.appendLog("去重后共有 %d 名用户", len(unique))
	app.appendLog("")
	app.appendLog("# ----------------------------------------------------")
	app.appendLog("开始核对资格")
	app.appendLog("# ----------------------------------------------------")

	app.setProgress(func(p *lotteryProgress) {
		p.Stage = stageFilter
		p.StageText = "正在核对抽奖资格"
		p.UsersTotal = len(unique)
	})

	// 两种判定方式（由「排除隐藏粉丝团灯牌的用户」切换）：
	//   不勾 = 用"评论区的粉丝团灯牌"：评论接口自带的 member.fans_detail 与评论区显示一致，
	//          对"拥有该粉丝团灯牌"的人都有效，无论是否佩戴、是否隐藏
	//   勾上 = 只看"用户个人的粉丝团灯牌列表"（勋章墙 MedalWall）：
	//          评论区数据一概不看，隐藏了粉丝团灯牌 / 只显示当前佩戴的都会被漏判
	if config.RequireMedal {
		app.appendLog("粉丝团灯牌核对方式：%s", medalCheckLabel(config))
	}

	candidates := make([]LotteryCandidate, 0, len(unique))
	index := 0
	total := len(unique)
	medalFromComment, medalFromList := 0, 0
	for _, user := range unique {
		index++

		// report 把本轮检查结果推给可视化面板；passed 为 true 时才会进入候选池
		report := func(passed bool, reason string, medal MedalInfo) {
			app.reportProgressUser(progressUser{
				UID:        user.UID,
				Name:       user.Name,
				Avatar:     previewAvatar(user.AvatarURL),
				Level:      user.Level,
				MedalName:  medal.MedalName,
				MedalLevel: medal.Level,
				Passed:     passed,
				Reason:     reason,
				Seq:        index,
			})
		}

		// UP主本人不参与抽奖：直接排除，
		// 既不进候选池，也不会出现在抽奖动效里
		if video.UPUID > 0 && user.UID == video.UPUID {
			app.appendLog("[%5d/%d] ✗ %s (UID=%d) UP主本人不参与抽奖", index, total, user.Name, user.UID)
			report(false, "UP主本人不参与抽奖", MedalInfo{})
			continue
		}

		if config.RequireLevel && user.Level < config.MinLevel {
			report(false, fmt.Sprintf("等级 Lv.%d 低于 Lv.%d", user.Level, config.MinLevel), MedalInfo{})
			continue
		}

		if !config.RequireMedal {
			candidates = append(candidates, LotteryCandidate{
				UID:       user.UID,
				Name:      user.Name,
				Content:   user.Content,
				Level:     user.Level,
				AvatarURL: app.avatarForUserWithFallback(user.UID, user.AvatarURL),
			})
			report(true, "", MedalInfo{})
			continue
		}

		// 按所选方式取粉丝团灯牌：勾了「排除隐藏粉丝团灯牌的用户」就只查对方个人的
		// 粉丝团灯牌列表，否则用评论区的粉丝团灯牌。
		var medal MedalInfo
		var exists bool
		source := "评论区"
		if config.ExcludeHiddenList {
			source = "个人列表"
			var err error
			medal, exists, err = app.getUserMedal(user.UID, video.UPUID)
			if err != nil {
				app.appendLog("[%5d/%d] ✗ %s (UID=%d) 粉丝团灯牌查询失败：%v", index, total, user.Name, user.UID, err)
				report(false, "粉丝团灯牌查询失败", MedalInfo{})
				time.Sleep(medalDelay)
				continue
			}
			time.Sleep(medalDelay)
		} else {
			medal, exists = user.Medal, user.Medal.MedalName != ""
		}

		if !exists {
			app.appendLog("[%5d/%d] ✗ %s (UID=%d)", index, total, user.Name, user.UID)
			report(false, "没有该 UP 的粉丝团灯牌", MedalInfo{})
			continue
		}

		if config.ExcludeHiddenList {
			medalFromList++
		} else {
			medalFromComment++
		}

		if medal.Level < config.MinMedalLevel {
			app.appendLog("[%5d/%d] ✗ %s (UID=%d) 粉丝团灯牌：%s Lv.%d（%s）", index, total, user.Name, user.UID, medal.MedalName, medal.Level, source)
			report(false, fmt.Sprintf("粉丝团灯牌只有 Lv.%d", medal.Level), medal)
			time.Sleep(medalDelay)
			continue
		}

		candidates = append(candidates, LotteryCandidate{
			UID:        user.UID,
			Name:       user.Name,
			Content:    user.Content,
			Level:      user.Level,
			MedalID:    medal.MedalID,
			MedalName:  medal.MedalName,
			MedalLevel: medal.Level,
			AvatarURL:  app.avatarForUserWithFallback(user.UID, user.AvatarURL),
		})

		app.appendLog("[%5d/%d] ✓ %s (UID=%d) 粉丝团灯牌：%s Lv.%d（%s）", index, total, user.Name, user.UID, medal.MedalName, medal.Level, source)
		report(true, "", medal)
		time.Sleep(medalDelay)
	}

	if config.RequireMedal {
		if config.ExcludeHiddenList {
			app.appendLog("粉丝团灯牌来源：个人列表 %d 人", medalFromList)
		} else {
			app.appendLog("粉丝团灯牌来源：评论区 %d 人", medalFromComment)
		}
	}

	app.appendLog("# ----------------------------------------------------")
	app.appendLog("资格核对完成：%d 人符合条件", len(candidates))
	app.appendLog("# ----------------------------------------------------")
	if len(candidates) == 0 {
		app.appendLog("没有符合条件的用户")
		app.failProgress("没有任何用户符合抽奖条件")
		return
	}

	if config.DrawCount > len(candidates) {
		app.appendLog("中奖名额 %d 超过符合条件人数 %d，已自动调整为 %d", config.DrawCount, len(candidates), len(candidates))
		config.DrawCount = len(candidates)
	}

	app.setProgress(func(p *lotteryProgress) {
		p.Stage = stageDraw
		p.StageText = "正在进行随机抽取"
		p.DrawCount = config.DrawCount
		p.Candidates = len(candidates)
	})

	if err := secureShuffle(candidates); err != nil {
		app.appendLog("随机抽奖失败：%v", err)
		app.failProgress("随机抽奖失败：%v", err)
		return
	}

	app.appendLog("")
	app.appendLog("========================================")
	app.appendLog("抽奖结果")
	app.appendLog("========================================")

	for i, winner := range candidates[:config.DrawCount] {
		app.appendLog("第 %d 名：%s", i+1, winner.Name)
		app.appendLog("UID：%d", winner.UID)
		if config.RequireLevel {
			app.appendLog("用户等级：Lv.%d", winner.Level)
		}
		if config.RequireMedal {
			app.appendLog("粉丝团灯牌：%s Lv.%d", winner.MedalName, winner.MedalLevel)
		}
		app.appendLog("评论：%s", winner.Content)
		app.appendLog("----------------------------------------")
	}

	upAvatar := video.UPAvatar
	if upAvatar == "" {
		if face, err := app.getUserFace(video.UPUID); err == nil {
			upAvatar = normalizeAvatarURL(face)
		}
	}
	app.setResult(&LotteryResult{
		Done: true,
		Video: &lotteryVideo{
			BV:       video.BVID,
			Title:    video.Title,
			UPName:   video.UPName,
			UPUID:    video.UPUID,
			UPAvatar: upAvatar,
		},
		Candidates: candidates,
		Winners:    append([]LotteryCandidate(nil), candidates[:config.DrawCount]...),
	})
	app.appendLog("开奖完成，中奖名单已生成")

	app.finishProgress(stageDone, fmt.Sprintf("抽奖完成，共 %d 人中奖", config.DrawCount))
}

func (app *App) getVideoInfo(bv string) (*VideoInfo, error) {
	params := url.Values{}
	params.Set("bvid", bv)

	body, _, err := app.httpGet(biliAPIBase+"/x/web-interface/view?"+params.Encode(), false)
	if err != nil {
		return nil, err
	}

	var r videoResponse
	if err = json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	if r.Code != 0 {
		return nil, fmt.Errorf("B站视频接口错误：%d %s", r.Code, r.Message)
	}

	v := &VideoInfo{
		AID:      r.Data.AID,
		BVID:     r.Data.BVID,
		Title:    r.Data.Title,
		UPName:   r.Data.Owner.Name,
		UPUID:    int64(r.Data.Owner.Mid),
		UPAvatar: normalizeAvatarURL(r.Data.Owner.Face),
	}

	app.appendLog("视频：%s", v.Title)
	app.appendLog("UP主：%s（UID：%d）", v.UPName, v.UPUID)
	app.appendLog("AID：%d", v.AID)
	return v, nil
}

// ---------------------------------------------------------------------------
// WBI 签名（新版接口需要 w_rid + wts）
// ---------------------------------------------------------------------------

const wbiKeyTTL = 12 * time.Hour

var mixinKeyEncTab = []int{
	46, 47, 18, 2, 53, 8, 23, 32, 15, 50, 10, 31, 58, 3, 45, 35,
	27, 43, 5, 49, 33, 9, 42, 19, 29, 28, 14, 39, 12, 38, 41, 13,
	37, 48, 7, 16, 24, 55, 40, 61, 26, 17, 0, 1, 60, 51, 30, 4,
	22, 25, 54, 21, 56, 59, 6, 63, 57, 62, 11, 36, 20, 34, 44, 52,
}

type wbiKeys struct {
	imgKey string
	subKey string
	at     time.Time
}

// wbiKeyFromURL 从 https://i0.hdslb.com/bfs/wbi/xxxx.png 里取出 xxxx
func wbiKeyFromURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	name := raw
	if i := strings.LastIndex(raw, "/"); i >= 0 {
		name = raw[i+1:]
	}
	if i := strings.Index(name, "."); i >= 0 {
		name = name[:i]
	}
	return name
}

// mixinKey 按固定顺序表重组 img_key + sub_key，取前 32 位
func mixinKey(imgKey, subKey string) string {
	raw := imgKey + subKey
	if len(raw) < 64 {
		return ""
	}
	buf := make([]byte, 0, len(mixinKeyEncTab))
	for _, idx := range mixinKeyEncTab {
		if idx >= 0 && idx < len(raw) {
			buf = append(buf, raw[idx])
		}
	}
	if len(buf) > 32 {
		buf = buf[:32]
	}
	return string(buf)
}

// wbiFilterValue 签名前需要剔除的字符
func wbiFilterValue(v string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '!', '\'', '(', ')', '*':
			return -1
		}
		return r
	}, v)
}

// wbiEscape 按 WBI 规则做 URL 编码（空格用 %20 而不是 +）
func wbiEscape(v string) string {
	return strings.ReplaceAll(url.QueryEscape(v), "+", "%20")
}

// wbiSign 计算 w_rid。params 里需要已经包含 wts。
func wbiSign(params url.Values, mixKey string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		if k == "w_rid" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte('&')
		}
		sb.WriteString(wbiEscape(k))
		sb.WriteByte('=')
		sb.WriteString(wbiEscape(wbiFilterValue(params.Get(k))))
	}

	sum := md5.Sum([]byte(sb.String() + mixKey))
	return hex.EncodeToString(sum[:])
}

func (app *App) getWbiMixinKey() (string, error) {
	app.wbiMu.Lock()
	if app.wbi != nil && time.Since(app.wbi.at) < wbiKeyTTL {
		key := mixinKey(app.wbi.imgKey, app.wbi.subKey)
		app.wbiMu.Unlock()
		if key != "" {
			return key, nil
		}
	} else {
		app.wbiMu.Unlock()
	}

	body, _, err := app.httpGet(biliAPIBase+"/x/web-interface/nav", false)
	if err != nil {
		return "", err
	}

	var r struct {
		Code int `json:"code"`
		Data struct {
			WbiImg struct {
				ImgURL string `json:"img_url"`
				SubURL string `json:"sub_url"`
			} `json:"wbi_img"`
		} `json:"data"`
	}
	if err = json.Unmarshal(body, &r); err != nil {
		return "", err
	}

	imgKey := wbiKeyFromURL(r.Data.WbiImg.ImgURL)
	subKey := wbiKeyFromURL(r.Data.WbiImg.SubURL)
	key := mixinKey(imgKey, subKey)
	if key == "" {
		return "", errors.New("未能获取到 WBI 密钥")
	}

	app.wbiMu.Lock()
	app.wbi = &wbiKeys{imgKey: imgKey, subKey: subKey, at: time.Now()}
	app.wbiMu.Unlock()

	return key, nil
}

// replyPage 请求一页一级评论：offset 非空时用游标翻页，否则用 pn 页码。
func (app *App) replyPage(aid int64, pn int, offset string) (*commentResponse, error) {
	p := url.Values{}
	p.Set("oid", strconv.FormatInt(aid, 10))
	p.Set("type", "1")
	p.Set("sort", "0")
	p.Set("ps", strconv.Itoa(commentPageSize))
	p.Set("nohot", "1")
	p.Set("plat", "1")
	p.Set("web_location", "1315875")
	if offset != "" {
		p.Set("pagination_str", `{"offset":"`+offset+`"}`)
	} else {
		p.Set("pn", strconv.Itoa(pn))
	}

	body, _, err := app.httpGet(biliAPIBase+"/x/v2/reply?"+p.Encode(), true)
	if err != nil {
		return nil, err
	}

	var r commentResponse
	if err = json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	if r.Code != 0 {
		return nil, fmt.Errorf("B站评论接口错误：%d %s", r.Code, r.Message)
	}
	return &r, nil
}

// replyPageCursor 用新版评论区接口（带 WBI 签名）按游标取一页。
// 页码翻页（pn）只能翻到第 5 页左右，深翻页必须走这里。
func (app *App) replyPageCursor(aid int64, offset string, next int) (*commentResponse, error) {
	mixKey, err := app.getWbiMixinKey()
	if err != nil {
		return nil, err
	}

	p := url.Values{}
	p.Set("oid", strconv.FormatInt(aid, 10))
	p.Set("type", "1")
	p.Set("mode", "3")
	p.Set("next", strconv.Itoa(next))
	p.Set("ps", strconv.Itoa(commentPageSize))
	p.Set("plat", "1")
	p.Set("web_location", "1315875")
	p.Set("pagination_str", `{"offset":"`+offset+`"}`)
	p.Set("wts", strconv.FormatInt(time.Now().Unix(), 10))
	p.Set("w_rid", wbiSign(p, mixKey))

	body, _, err := app.httpGet(biliAPIBase+"/x/v2/reply/wbi/main?"+p.Encode(), true)
	if err != nil {
		return nil, err
	}

	var r commentResponse
	if err = json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	if r.Code != 0 {
		return nil, fmt.Errorf("B站新版评论接口错误：%d %s", r.Code, r.Message)
	}
	return &r, nil
}

// nextCursorOffset 从游标信息里取出下一个 offset
func nextCursorOffset(c commentCursor) string {
	if o := strings.TrimSpace(c.PaginationReply.NextOffset); o != "" {
		return o
	}
	if s := strings.TrimSpace(c.PaginationStr); s != "" {
		var box struct {
			Offset string `json:"offset"`
		}
		if err := json.Unmarshal([]byte(s), &box); err == nil {
			return strings.TrimSpace(box.Offset)
		}
	}
	return ""
}

// getComments 抓取一级评论（可选二级评论）。
// onPage 用于向可视化面板上报进度：已抓取条数、评论总数、当前页、总页数。
func (app *App) getComments(aid int64, includeSubComments bool, onPage func(fetched, total, page, pages int)) ([]Comment, error) {
	var all []Comment

	seen := make(map[int64]bool)
	rootCount := 0
	rootTotal := 0
	nextOffset := ""
	useCursor := true
	cursorConfirmed := false

	for page := 1; page <= maxCommentPages; page++ {
		var (
			r   *commentResponse
			err error
		)

		if useCursor {
			r, err = app.replyPageCursor(aid, nextOffset, page-1)
			if err != nil {
				if cursorConfirmed {
					break
				}
				useCursor = false
			} else {
				cursorConfirmed = true
			}
		}

		if !useCursor {
			r, err = app.replyPage(aid, page, "")
			if err != nil {
				return nil, err
			}
		}

		// 两个接口报告的总数口径可能不同，取较大者
		if c := r.Data.Page.Count; c > 0 {
			rootTotal = c
		}
		if c := r.Data.Cursor.AllCount; c > rootTotal {
			rootTotal = c
		}

		// 置顶评论常只出现在 top_replies / upper.top，合并后按 RPID 去重
		replies := make([]Comment, 0, len(r.Data.Replies)+len(r.Data.TopReplies)+1)
		replies = append(replies, r.Data.Replies...)
		replies = append(replies, r.Data.TopReplies...)
		if top := r.Data.Upper.Top; top != nil && top.RPID != 0 {
			replies = append(replies, *top)
		}

		if len(replies) == 0 {
			break
		}

		fresh := make([]Comment, 0, len(replies))
		for _, c := range replies {
			if c.RPID != 0 {
				if seen[c.RPID] {
					continue
				}
				seen[c.RPID] = true
			}
			fresh = append(fresh, c)
		}
		if len(fresh) == 0 {
			break
		}

		all = append(all, fresh...)
		rootCount += len(fresh)

		app.appendLog("正在获取一级评论：第 %d 页，本页 %d 条，累计 %d 条", page, len(fresh), len(all))
		app.logComments(fresh, "一级评论")

		pages := 0
		if rootTotal > 0 {
			pages = (rootTotal + commentPageSize - 1) / commentPageSize
		}

		// report 上报抓取进度（总数取接口报告值与已抓取值的较大者）
		report := func() {
			if onPage == nil {
				return
			}
			known := rootTotal
			if len(all) > known {
				known = len(all)
			}
			onPage(len(all), known, page, pages)
		}

		report()

		// 先把本页所有一级评论的全部二级回复抓完、按楼归组，
		// 等整页数据齐了再一次性推给弹幕：同一楼的所有回复到齐后才整体显示
		subsByRoot := make(map[int64][]Comment)
		if includeSubComments {
			withReplies := 0
			for _, root := range fresh {
				// 只有确实有回复的楼才去请求：没有二级评论的楼返回的也是空结果，
				// 请求过去纯属浪费
				if root.RCount <= 0 {
					continue
				}
				withReplies++

				subs, err := app.getSubComments(aid, root.RPID)
				if err != nil {
					app.appendLog("  [二级评论] 根评论 RPID=%d 获取失败：%v", root.RPID, err)
					continue
				}
				if len(subs) > 0 {
					subsByRoot[root.RPID] = subs
					all = append(all, subs...)
					app.appendLog("  [二级评论] 根评论 RPID=%d 获取 %d 条", root.RPID, len(subs))
					app.logComments(subs, "二级评论")
					app.setProgress(func(p *lotteryProgress) {
						p.SubFetched += len(subs)
						p.CommentsFetched = len(all)
						if p.CommentsTotal < len(all) {
							p.CommentsTotal = len(all)
						}
					})
				}
			}

			app.appendLog("  [二级评论] 本页 %d 楼，其中 %d 楼有回复", len(fresh), withReplies)

			// 二级评论抓完后同步一次
			report()
		}

		app.reportProgressCommentsGrouped(fresh, subsByRoot)

		// 只信任游标接口的 is_end：页码接口的 is_end 会跟着 pn 深度上限一起变 true
		if useCursor && r.Data.Cursor.IsEnd {
			break
		}

		if rootTotal > 0 && rootCount >= rootTotal {
			break
		}

		if useCursor {
			offset := nextCursorOffset(r.Data.Cursor)
			if offset == "" {
				break
			}
			nextOffset = offset
		} else if rootTotal == 0 && len(fresh) < commentPageSize {
			break
		}

		time.Sleep(commentPageDelay)
	}

	app.appendLog("评论收集完成，共 %d 条评论", len(all))
	return all, nil
}

func (app *App) getSubComments(aid int64, root int64) ([]Comment, error) {
	var all []Comment
	page := 1

	for {
		p := url.Values{}
		p.Set("oid", strconv.FormatInt(aid, 10))
		p.Set("type", "1")
		p.Set("root", strconv.FormatInt(root, 10))
		p.Set("ps", strconv.Itoa(commentPageSize))
		p.Set("pn", strconv.Itoa(page))

		body, _, err := app.httpGet(biliAPIBase+"/x/v2/reply/reply?"+p.Encode(), true)
		if err != nil {
			return nil, err
		}

		var r commentResponse
		if err = json.Unmarshal(body, &r); err != nil {
			return nil, err
		}
		if r.Code != 0 {
			return nil, fmt.Errorf("B站二级评论接口错误：%d %s", r.Code, r.Message)
		}

		replies := r.Data.Replies
		if len(replies) == 0 {
			break
		}
		all = append(all, replies...)

		// 与一级评论同理：单页返回数少于 ps 并不代表已经到底
		if r.Data.Page.Count > 0 {
			if len(all) >= r.Data.Page.Count {
				break
			}
		} else if len(replies) < commentPageSize {
			break
		}

		if page >= maxCommentPages {
			break
		}

		page++
		time.Sleep(commentPageDelay)
	}

	// 接口偶有不带 root 的情况：统一补成请求时的根评论 RPID，
	// 前端靠它把二级弹幕挂到对应的一级弹幕下面
	for i := range all {
		if int64(all[i].Root) == 0 {
			all[i].Root = FlexibleInt64(root)
		}
	}

	return all, nil
}

func (app *App) logComments(comments []Comment, label string) {
	for _, c := range comments {
		message := c.Content.Message
		runes := []rune(message)
		if len(runes) > 100 {
			message = string(runes[:100]) + "..."
		}
		app.appendLog("  [%s] %s | UID=%d | Lv.%d | %s",
			label,
			c.Member.Name,
			int64(c.Member.Mid),
			c.Member.LevelInfo.CurrentLevel,
			message,
		)
	}
}

// getUserMedal 查"用户个人的粉丝团灯牌列表"（勋章墙 MedalWall），
// 返回该用户名下是否有一枚属于 targetUID 的粉丝团灯牌。
// 注意：列表被用户设为隐藏、或设为"仅显示当前佩戴"时，这里查不到。
func (app *App) getUserMedal(uid, targetUID int64) (MedalInfo, bool, error) {
	p := url.Values{}
	p.Set("target_id", strconv.FormatInt(uid, 10))

	body, _, err := app.httpGet(biliLiveAPI+"/xlive/web-ucenter/user/MedalWall?"+p.Encode(), true)
	if err != nil {
		return MedalInfo{}, false, err
	}

	var r medalResponse
	if err = json.Unmarshal(body, &r); err != nil {
		return MedalInfo{}, false, err
	}
	if r.Code != 0 {
		return MedalInfo{}, false, fmt.Errorf("B站粉丝团灯牌接口错误：%d %s", r.Code, r.Message)
	}

	for _, item := range r.Data.List {
		// 只要求拥有当前视频 UP 的粉丝团灯牌，不要求当前佩戴。
		if int64(item.MedalInfo.TargetID) != targetUID {
			continue
		}
		return item.MedalInfo, true, nil
	}

	return MedalInfo{}, false, nil
}

type spaceAccInfoResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Mid  int64  `json:"mid"`
		Name string `json:"name"`
		Face string `json:"face"`
	} `json:"data"`
}

func (app *App) getUserFace(uid int64) (string, error) {
	p := url.Values{}
	p.Set("mid", strconv.FormatInt(uid, 10))
	body, _, err := app.httpGet(biliAPIBase+"/x/space/acc/info?"+p.Encode(), true)
	if err != nil {
		return "", err
	}
	var r spaceAccInfoResponse
	if err = json.Unmarshal(body, &r); err != nil {
		return "", err
	}
	if r.Code != 0 {
		return "", fmt.Errorf("B站用户接口错误：%d %s", r.Code, r.Message)
	}
	return r.Data.Face, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func normalizeAvatarURL(face string) string {
	face = strings.TrimSpace(face)
	if face == "" {
		return ""
	}
	if strings.HasPrefix(face, "//") {
		return "https:" + face
	}
	return face
}

// ---------------------------------------------------------------------------
// 头像代理
//
// B站图片 CDN（*.hdslb.com）存在防盗链：当请求的 Referer 不属于 B 站域名时，
// 浏览器直接以 <img src="https://i0.hdslb.com/..."> 引用图片会拿到 403，
// 页面上就表现为“头像显示不出来”（只剩裂图/alt 文字）。
// 这里改由本地服务代取图片（带上正确的 Referer），再转发给前端，
// 取不到时返回一个按昵称首字生成的 SVG 占位头像，前端不会出现裂图。
// ---------------------------------------------------------------------------

const (
	avatarCacheTTL    = 6 * time.Hour
	avatarMaxSize     = 4 << 20
	avatarFetchTime   = 15 * time.Second
	avatarCacheMaxLen = 300
)

type avatarCacheEntry struct {
	data        []byte
	contentType string
	at          time.Time
}

var biliImageHostSuffixes = []string{"hdslb.com", "bilibili.com", "biliimg.com", "bilivideo.com"}

func isBiliImageHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return false
	}
	for _, suffix := range biliImageHostSuffixes {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

func (app *App) handleAvatar(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	data, contentType, err := app.fetchAvatar(r.URL.Query().Get("u"))
	if err != nil || len(data) == 0 {
		writePlaceholderAvatar(w, r.URL.Query().Get("n"))
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(data)
}

func (app *App) fetchAvatar(rawURL string) ([]byte, string, error) {
	target := normalizeAvatarURL(rawURL)
	if target == "" {
		return nil, "", errors.New("头像地址为空")
	}

	u, err := url.Parse(target)
	if err != nil {
		return nil, "", err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, "", fmt.Errorf("不支持的头像协议：%s", u.Scheme)
	}
	// 只允许 B 站自己的图片域名，避免被当成任意请求的跳板。
	if !isBiliImageHost(u.Hostname()) {
		return nil, "", fmt.Errorf("不允许的头像来源：%s", u.Hostname())
	}
	u.Scheme = "https"
	key := u.String()

	app.avatarMu.Lock()
	entry, ok := app.avatarCache[key]
	app.avatarMu.Unlock()
	if ok && time.Since(entry.at) < avatarCacheTTL {
		return entry.data, entry.contentType, nil
	}

	// 优先走 CDN 缩略图：页面里头像最大也只显示 ~144px（@2x），
	// 直接取缩略图能让浏览器少解码一个数量级的像素，内存和解码压力都小；
	// 万一缩略参数不被支持，再回退到原地址。
	thumbURL := thumbAvatarURL(u)
	data, contentType, err := downloadAvatarImage(thumbURL)
	if err != nil && thumbURL != key {
		data, contentType, err = downloadAvatarImage(key)
	}
	if err != nil {
		return nil, "", err
	}

	app.avatarMu.Lock()
	if app.avatarCache == nil {
		app.avatarCache = make(map[string]avatarCacheEntry)
	}
	if len(app.avatarCache) >= avatarCacheMaxLen {
		for k := range app.avatarCache {
			delete(app.avatarCache, k)
			break
		}
	}
	app.avatarCache[key] = avatarCacheEntry{data: data, contentType: contentType, at: time.Now()}
	app.avatarMu.Unlock()

	return data, contentType, nil
}

// thumbAvatarURL 给 B 站 hdslb 图床地址追加缩略参数（192px 方图 webp）。
// 已经带过“@”处理参数或 query 的地址保持原样，避免拼接出错。
func thumbAvatarURL(u *url.URL) string {
	if !strings.HasSuffix(strings.ToLower(u.Hostname()), "hdslb.com") {
		return u.String()
	}
	if strings.Contains(u.Path, "@") || u.RawQuery != "" {
		return u.String()
	}
	return u.String() + "@192w_192h_1c.webp"
}

func downloadAvatarImage(urlStr string) ([]byte, string, error) {
	req, err := http.NewRequest(http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", userAgent)
	// 关键：带上 B 站自身的 Referer，绕过图片 CDN 的防盗链。
	req.Header.Set("Referer", "https://www.bilibili.com/")
	req.Header.Set("Accept", "image/avif,image/webp,image/apng,image/*,*/*;q=0.8")

	client := &http.Client{Timeout: avatarFetchTime}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("下载头像失败：HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, avatarMaxSize))
	if err != nil {
		return nil, "", err
	}

	contentType := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "image/") {
		contentType = "image/jpeg"
	}
	return data, contentType, nil
}

func writePlaceholderAvatar(w http.ResponseWriter, name string) {
	initial := "?"
	for _, r := range strings.TrimSpace(name) {
		initial = string(r)
		break
	}

	svg := fmt.Sprintf(
		`<svg xmlns="http://www.w3.org/2000/svg" width="128" height="128">`+
			`<rect width="128" height="128" rx="64" fill="#8a919c"/>`+
			`<text x="64" y="80" text-anchor="middle" font-size="58" font-family="Arial,Helvetica,sans-serif" fill="#ffffff">%s</text>`+
			`</svg>`,
		html.EscapeString(initial),
	)

	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=600")
	_, _ = w.Write([]byte(svg))
}

func (app *App) avatarForUser(uid int64) string {
	return app.avatarForUserWithFallback(uid, "")
}

func (app *App) avatarForUserWithFallback(uid int64, preferred string) string {
	if avatar := normalizeAvatarURL(preferred); avatar != "" {
		return avatar
	}
	face, err := app.getUserFace(uid)
	if err != nil {
		return ""
	}
	return normalizeAvatarURL(face)
}

func secureShuffle[T any](items []T) error {
	for i := len(items) - 1; i > 0; i-- {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			return err
		}
		j := int(n.Int64())
		items[i], items[j] = items[j], items[i]
	}
	return nil
}

func (app *App) generateQRCode() (string, string, error) {
	body, _, err := app.httpGet(biliPassport+"/x/passport-login/web/qrcode/generate", false)
	if err != nil {
		return "", "", err
	}

	var r qrGenerateResponse
	if err = json.Unmarshal(body, &r); err != nil {
		return "", "", err
	}
	if r.Code != 0 {
		return "", "", fmt.Errorf("B站二维码接口错误：%d %s", r.Code, r.Message)
	}
	if r.Data.URL == "" || r.Data.QRCodeKey == "" {
		return "", "", errors.New("B站二维码接口返回数据不完整")
	}
	return r.Data.QRCodeKey, r.Data.URL, nil
}

func (app *App) createQRImage(qrURL string) (string, error) {
	file := filepath.Join(os.TempDir(), fmt.Sprintf("bili_lottery_qr_%d.png", time.Now().UnixNano()))
	if err := qrcode.WriteFile(qrURL, qrcode.Medium, 400, file); err != nil {
		return "", err
	}
	return file, nil
}

func (app *App) pollQRCode(qrKey string) (string, error) {
	p := url.Values{}
	p.Set("qrcode_key", qrKey)
	p.Set("source", "main-fe-header")
	apiURL := biliPassport + "/x/passport-login/web/qrcode/poll?" + p.Encode()

	for {
		body, cookies, err := app.httpGet(apiURL, false)
		if err != nil {
			return "", err
		}

		var r qrPollResponse
		if err = json.Unmarshal(body, &r); err != nil {
			return "", err
		}
		if r.Code != 0 {
			// 例如触发风控时根 code 非 0、data 为空，此时不能误判成"登录成功"
			return "", fmt.Errorf("B站登录接口错误：%d %s", r.Code, r.Message)
		}

		switch r.Data.Code {
		case 86101:
			app.appendLog("等待扫码")
		case 86090:
			app.appendLog("已扫码，请在 Bilibili APP 确认")
		case 86038:
			return "", errors.New("二维码已过期，请重新扫码")
		case 0:
			if s := extractSessdataFromCookies(cookies); s != "" {
				return s, nil
			}
			if r.Data.URL != "" {
				s, e := app.followLoginURL(r.Data.URL)
				if e == nil && s != "" {
					return s, nil
				}
			}
			return "", errors.New("登录成功，但未获取到 SESSDATA")
		default:
			if r.Data.Message != "" {
				app.appendLog("登录状态：%s", r.Data.Message)
			}
		}

		time.Sleep(2 * time.Second)
	}
}

func (app *App) followLoginURL(loginURL string) (string, error) {
	current := loginURL
	client := &http.Client{
		Timeout: 20 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	for i := 0; i < 10; i++ {
		req, err := http.NewRequest(http.MethodGet, current, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Referer", "https://www.bilibili.com/")

		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}

		if s := extractSessdataFromCookies(resp.Cookies()); s != "" {
			resp.Body.Close()
			return s, nil
		}

		loc := resp.Header.Get("Location")
		resp.Body.Close()
		if loc == "" {
			break
		}

		u, err := url.Parse(current)
		if err != nil {
			return "", err
		}
		n, err := url.Parse(loc)
		if err != nil {
			return "", err
		}
		current = u.ResolveReference(n).String()
	}

	return "", errors.New("登录跳转过程中未获取到 SESSDATA")
}

func extractSessdataFromCookies(cookies []*http.Cookie) string {
	for _, c := range cookies {
		if c != nil && strings.EqualFold(c.Name, "SESSDATA") {
			return c.Value
		}
	}
	return ""
}

func (app *App) appendLog(format string, args ...interface{}) {
	message := fmt.Sprintf(format, args...)
	app.logMu.Lock()
	defer app.logMu.Unlock()

	app.logs = append(app.logs, message)
	if len(app.logs) > 1000 {
		app.logs = app.logs[len(app.logs)-1000:]
	}
}

func (app *App) setSessdata(value string) {
	app.mu.Lock()
	app.sessdata = strings.TrimSpace(value)
	app.mu.Unlock()
}

func (app *App) getSessdata() string {
	app.mu.RLock()
	defer app.mu.RUnlock()
	return app.sessdata
}

func validateConfig(req lotteryRequest) (LotteryConfig, error) {
	if req.RequireLevel && req.MinLevel < 0 {
		return LotteryConfig{}, errors.New("用户等级不能小于 0")
	}
	if req.RequireMedal && req.MinMedalLevel < 1 {
		return LotteryConfig{}, errors.New("粉丝团灯牌等级不能小于 1")
	}
	if req.DrawCount < 1 {
		return LotteryConfig{}, errors.New("抽取人数必须大于 0")
	}

	return LotteryConfig{
		RequireLevel:       req.RequireLevel,
		MinLevel:           req.MinLevel,
		RequireMedal:       req.RequireMedal,
		MinMedalLevel:      req.MinMedalLevel,
		ExcludeHiddenList:  req.ExcludeHiddenList,
		IncludeSubComments: req.IncludeSubComments,
		DrawCount:          req.DrawCount,
	}, nil
}

// medalCheckLabel 把粉丝团灯牌核对方式转成日志里好读的中文
func medalCheckLabel(config LotteryConfig) string {
	if config.ExcludeHiddenList {
		return "只看对方个人的粉丝团灯牌列表（隐藏 / 只显示当前佩戴的会被漏判）"
	}
	return "用评论区的粉丝团灯牌（最全：含未佩戴、已隐藏的用户）"
}

func (app *App) httpGet(rawURL string, needLogin bool) ([]byte, []*http.Cookie, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, nil, err
	}

	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", "https://www.bilibili.com/")

	if needLogin {
		sessdata := app.getSessdata()
		if sessdata == "" {
			return nil, nil, errors.New("未登录 Bilibili")
		}
		req.Header.Set("Cookie", "SESSDATA="+sessdata)
	}

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	cookies := resp.Cookies()
	if err != nil {
		return nil, cookies, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return body, cookies, fmt.Errorf("HTTP状态码：%d", resp.StatusCode)
	}

	return body, cookies, nil
}

func (app *App) deleteQRFile() {
	app.qrMu.Lock()
	defer app.qrMu.Unlock()
	if app.qrFile != "" {
		_ = os.Remove(app.qrFile)
		app.qrFile = ""
	}
}

func (app *App) cleanup() {
	app.setSessdata("")
	app.deleteQRFile()
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}
