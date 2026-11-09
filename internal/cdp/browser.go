// browser.go 拉起带调试端口的浏览器实例，并从 CDP 读回 Cookie。
//
// 设计取舍（2026-09-26 实测）：
//
//	为什么不直接读浏览器 Cookie 数据库：
//	  Edge/Chrome 运行时对 `User Data/<Profile>/Network/Cookies` 持独占锁，
//	  实测既不能读也不能复制（20 个进程在跑）；且 Cookie 值为 DPAPI + AES-GCM 加密。
//	  CDP 是唯一稳定路径。
//
//	为什么用独立 profile 而不是 InPrivate：
//	  独立 profile 天然完全隔离（Cookie/LocalStorage/缓存全独立），
//	  比 InPrivate 更适合「多账号逐个添加」——每个账号一个全新 profile，
//	  互不干扰，也绝不触碰用户真实浏览器的登录态。
package cdp

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Cookie 一个浏览器 Cookie（只保留渠道登录实际消费的字段）。
type Cookie struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain"`
}

// Browser 一个受控的浏览器实例。
//
// 两种来源：Launch 自己拉起一个（cmd/profile 非空，Close 会杀进程并清目录），
// 或 Attach 接管一个**已经在跑**的浏览器（用户自己的 Chrome/Edge，开着调试端口）。
// 后者的语义差别很大：绝不能关掉用户的浏览器 —— 这正是 attached 标记存在的理由。
type Browser struct {
	cmd      *exec.Cmd
	port     int
	profile  string
	exe      string
	attached bool
	wsURL    string // 浏览器级 WS 地址；Attach 时取自 /json/version
}

// Options 启动选项。
type Options struct {
	// ExePath 浏览器可执行文件路径；空则自动探测。
	ExePath string
	// ProfileDir 用户数据目录；空则创建临时目录。
	ProfileDir string
	// StartURL 启动后打开的地址。
	StartURL string
	// Port 调试端口；0 则自动选空闲端口。
	Port int
}

// browserCandidates 按平台给出候选浏览器路径（优先 Edge，Windows 自带）。
func browserCandidates() []string {
	switch runtime.GOOS {
	case "windows":
		local := os.Getenv("LOCALAPPDATA")
		pf := os.Getenv("ProgramFiles")
		pf86 := os.Getenv("ProgramFiles(x86)")
		return []string{
			filepath.Join(pf86, `Microsoft\Edge\Application\msedge.exe`),
			filepath.Join(pf, `Microsoft\Edge\Application\msedge.exe`),
			filepath.Join(pf, `Google\Chrome\Application\chrome.exe`),
			filepath.Join(pf86, `Google\Chrome\Application\chrome.exe`),
			filepath.Join(local, `Google\Chrome\Application\chrome.exe`),
		}
	case "darwin":
		return []string{
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		}
	default:
		return []string{
			"/usr/bin/microsoft-edge",
			"/usr/bin/google-chrome",
			"/usr/bin/chromium",
			"/usr/bin/chromium-browser",
		}
	}
}

// FindBrowser 返回可用的浏览器路径。
func FindBrowser() (string, error) {
	for _, p := range browserCandidates() {
		if p == "" {
			continue
		}
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("未找到 Edge/Chrome，请在设置中手动指定浏览器路径")
}

// freePort 选一个空闲 TCP 端口。
func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// Launch 拉起浏览器并等待 CDP 就绪。
func Launch(ctx context.Context, opts Options) (*Browser, error) {
	exe := opts.ExePath
	if exe == "" {
		var err error
		exe, err = FindBrowser()
		if err != nil {
			return nil, err
		}
	}

	port := opts.Port
	if port == 0 {
		var err error
		port, err = freePort()
		if err != nil {
			return nil, fmt.Errorf("cdp: 分配调试端口失败: %w", err)
		}
	}

	profile := opts.ProfileDir
	if profile == "" {
		var err error
		profile, err = os.MkdirTemp("", "wildwork-cdp-*")
		if err != nil {
			return nil, fmt.Errorf("cdp: 创建临时 profile 失败: %w", err)
		}
	}

	startURL := opts.StartURL
	if startURL == "" {
		startURL = "about:blank"
	}

	args := []string{
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--user-data-dir=" + profile,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-features=Translate,OptimizationHints",
		startURL,
	}
	cmd := exec.CommandContext(ctx, exe, args...)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("cdp: 启动浏览器失败: %w", err)
	}

	b := &Browser{cmd: cmd, port: port, profile: profile, exe: exe}

	// 等 CDP 就绪（浏览器冷启动可能要几秒）
	if err := b.waitReady(ctx, 30*time.Second); err != nil {
		_ = b.Close()
		return nil, err
	}
	return b, nil
}

// waitReady 轮询 /json/version 直到 CDP 可用。
func (b *Browser) waitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 3 * time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d/json/version", b.port)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(400 * time.Millisecond)
	}
	return fmt.Errorf("cdp: 浏览器调试端口 %d 未就绪（超时 %s）", b.port, timeout)
}

// Port 返回调试端口。
func (b *Browser) Port() int { return b.port }

// ProfileDir 返回使用的 profile 目录。
func (b *Browser) ProfileDir() string { return b.profile }

// Close 关闭浏览器并清理临时 profile。
func (b *Browser) Close() error {
	// 接管的浏览器属于用户：只断开引用，不关窗口、不杀进程、不动 profile。
	if b.attached {
		return nil
	}
	// 优先经 CDP 优雅关闭，避免留下进程
	if ws, err := DialWebSocket(b.browserWSURL(), 5*time.Second); err == nil {
		_, _ = ws.Call("Browser.close", nil, 3*time.Second)
		_ = ws.Close()
		time.Sleep(1500 * time.Millisecond)
	}
	if b.cmd != nil && b.cmd.Process != nil {
		_ = b.cmd.Process.Kill()
		_, _ = b.cmd.Process.Wait()
	}
	// 清理 profile（浏览器可能还持有句柄，失败不致命）
	for i := 0; i < 5; i++ {
		if err := os.RemoveAll(b.profile); err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	return nil
}

// browserWSURL 取浏览器级 WebSocket 地址。
func (b *Browser) browserWSURL() string {
	if b.wsURL != "" {
		return b.wsURL
	}
	return fmt.Sprintf("ws://127.0.0.1:%d/devtools/browser", b.port)
}

// Attach 接管一个已在运行的浏览器（必须已开 --remote-debugging-port）。
//
// endpoint 形如 http://127.0.0.1:9222（也接受 127.0.0.1:9222）。与 Launch 的区别：
// 共用用户日常 profile 里的登录态 —— 用户在自己熟悉的浏览器里登录一次，
// 渠道就能直接读到 Cookie，不必再复制粘贴。代价是浏览器归用户所有：
// Close 只解引用，绝不关闭它。
func Attach(ctx context.Context, endpoint string) (*Browser, error) {
	ep := strings.TrimSpace(endpoint)
	if ep == "" {
		return nil, fmt.Errorf("cdp: 浏览器调试地址为空")
	}
	if !strings.Contains(ep, "://") {
		ep = "http://" + ep
	}
	ep = strings.TrimRight(ep, "/")
	u, err := url.Parse(ep)
	if err != nil {
		return nil, fmt.Errorf("cdp: 非法调试地址 %q: %w", endpoint, err)
	}
	port := 0
	if p := u.Port(); p != "" {
		port, err = strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("cdp: 非法端口 %q: %w", p, err)
		}
	}

	client := &http.Client{Timeout: 4 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep+"/json/version", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cdp: 调试端口未响应（%s）：请确认浏览器以 --remote-debugging-port 启动: %w", ep, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cdp: %s/json/version 返回 HTTP %d", ep, resp.StatusCode)
	}
	var ver struct {
		Browser           string `json:"Browser"`
		WebSocketDebugger string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ver); err != nil {
		return nil, fmt.Errorf("cdp: 解析 /json/version 失败: %w", err)
	}
	wsURL := ver.WebSocketDebugger
	if wsURL == "" {
		wsURL = fmt.Sprintf("ws://%s/devtools/browser", u.Host)
	}
	return &Browser{port: port, attached: true, wsURL: wsURL}, nil
}

// Probe 只检查 endpoint 背后是不是一个可用的 CDP 调试端口（不做任何状态变更）。
func Probe(ctx context.Context, endpoint string) error {
	b, err := Attach(ctx, endpoint)
	if err != nil {
		return err
	}
	if b.wsURL == "" {
		return fmt.Errorf("cdp: %s 未返回浏览器级调试地址", endpoint)
	}
	return nil
}

// OpenTab 在浏览器里开一个新标签页并返回 targetId。
func (b *Browser) OpenTab(ctx context.Context, rawURL string) (string, error) {
	ws, err := DialWebSocket(b.browserWSURL(), 10*time.Second)
	if err != nil {
		return "", fmt.Errorf("cdp: 连接浏览器级 WS 失败: %w", err)
	}
	defer ws.Close()
	raw, err := ws.Call("Target.createTarget", map[string]any{"url": rawURL}, 10*time.Second)
	if err != nil {
		return "", fmt.Errorf("cdp: 新建标签页失败: %w", err)
	}
	var out struct {
		TargetID string `json:"targetId"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("cdp: 解析 createTarget 结果失败: %w", err)
	}
	return out.TargetID, nil
}

// pageTarget 一个可调试的页面目标。
type pageTarget struct {
	Type              string `json:"type"`
	URL               string `json:"url"`
	WebSocketDebugger string `json:"webSocketDebuggerUrl"`
}

// listTargets 列出 CDP 目标。
func (b *Browser) listTargets(ctx context.Context) ([]pageTarget, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/json/list", b.port), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out []pageTarget
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// pageWSURL 找到匹配 hostFragment 的页面目标；找不到时退回任意页面。
func (b *Browser) pageWSURL(ctx context.Context, hostFragment string) (string, error) {
	targets, err := b.listTargets(ctx)
	if err != nil {
		return "", err
	}
	var fallback string
	for _, t := range targets {
		if t.Type != "page" || t.WebSocketDebugger == "" {
			continue
		}
		if fallback == "" {
			fallback = t.WebSocketDebugger
		}
		if hostFragment != "" && strings.Contains(t.URL, hostFragment) {
			return t.WebSocketDebugger, nil
		}
	}
	if fallback != "" {
		return fallback, nil
	}
	return "", fmt.Errorf("cdp: 未找到可调试页面")
}

// AllCookies 读取浏览器当前全部 Cookie（经 Network.getAllCookies）。
//
// hostFragment 用于挑选页面目标（CDP 的 Cookie 是浏览器级的，但需要借一个
// 页面会话发命令）；传目标域名可提高命中率。
func (b *Browser) AllCookies(ctx context.Context, hostFragment string) ([]Cookie, error) {
	wsURL, err := b.pageWSURL(ctx, hostFragment)
	if err != nil {
		return nil, err
	}
	ws, err := DialWebSocket(wsURL, 10*time.Second)
	if err != nil {
		return nil, err
	}
	defer ws.Close()

	if _, err := ws.Call("Network.enable", nil, 10*time.Second); err != nil {
		return nil, fmt.Errorf("cdp: Network.enable 失败: %w", err)
	}
	raw, err := ws.Call("Network.getAllCookies", nil, 15*time.Second)
	if err != nil {
		return nil, fmt.Errorf("cdp: 读取 Cookie 失败: %w", err)
	}
	var out struct {
		Cookies []Cookie `json:"cookies"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("cdp: 解析 Cookie 失败: %w", err)
	}
	return out.Cookies, nil
}

// WaitForCookie 轮询等待指定 Cookie 出现且非空。
//
// 这是「自动获取登录态」的核心：拉起浏览器后调用本方法，
// 用户登录完成的那一刻 Cookie 出现，方法即返回。
func (b *Browser) WaitForCookie(ctx context.Context, hostFragment, cookieName string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		cookies, err := b.AllCookies(ctx, hostFragment)
		if err != nil {
			lastErr = err
		} else {
			for _, c := range cookies {
				if c.Name == cookieName && strings.TrimSpace(c.Value) != "" &&
					(hostFragment == "" || strings.Contains(c.Domain, hostFragment)) {
					return c.Value, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(1500 * time.Millisecond):
		}
	}
	if lastErr != nil {
		return "", fmt.Errorf("等待登录超时（%s）；最后一次错误: %w", timeout, lastErr)
	}
	return "", fmt.Errorf("等待登录超时（%s）：未捕获到 %s", timeout, cookieName)
}
