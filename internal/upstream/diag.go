// diag.go 内容拦截诊断：ErrContentBlocked 时把发往上游的 prepared body 落盘，
// 供 scripts/bisect-blocked.py 做分层重放与二分定位。正常请求零 IO、零日志——
// [diag] 行只在拦截时产生，是下次定位的入口。
//
// 零 token 口径：body 是发往上游的请求体，不含 Authorization 等凭据头，
// 落盘内容仅为请求体本身，符合 AGENTS.md「日志零 token」红线。
package upstream

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// diagDir 落盘目录（相对程序工作目录；main.go 启动时 Chdir 到 exe 所在目录/
// WILDWORK_HOME，故即部署目录下的 data/blocked/）。
const diagDir = "data/blocked"

// diagKeep 滚动保留份数。
const diagKeep = 3

// DumpBlocked 把被拦请求体写入 data/blocked/blocked-<unixnano>.json，
// 滚动保留最近 3 份。落盘失败只记日志，绝不影响请求返回。
func DumpBlocked(body []byte) {
	if len(body) == 0 {
		return
	}
	if err := os.MkdirAll(diagDir, 0o755); err != nil {
		log.Printf("[diag] 创建 %s 失败: %v", diagDir, err)
		return
	}
	p := filepath.Join(diagDir, fmt.Sprintf("blocked-%d.json", time.Now().UnixNano()))
	if err := os.WriteFile(p, body, 0o644); err != nil {
		log.Printf("[diag] 落盘失败: %v", err)
		return
	}
	log.Printf("[diag] 已被拦的请求体已保存: %s（%d 字节）", p, len(body))
	pruneBlocked(diagDir)
}

// pruneBlocked 滚动清理：文件名含 unixnano 时间戳，字典序即时间序，
// 只保留最新 diagKeep 份。
func pruneBlocked(dir string) {
	matches, err := filepath.Glob(filepath.Join(dir, "blocked-*.json"))
	if err != nil || len(matches) <= diagKeep {
		return
	}
	sort.Strings(matches)
	for _, old := range matches[:len(matches)-diagKeep] {
		if rmErr := os.Remove(old); rmErr != nil {
			log.Printf("[diag] 清理旧样本失败 %s: %v", old, rmErr)
		}
	}
}
