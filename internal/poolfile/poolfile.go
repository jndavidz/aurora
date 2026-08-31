// Package poolfile 提供"每行一个 token"池文件的原子回写(可靠性 A2)。
//
// 背景:GLM/Kimi 换发轮换后需把新 refresh_token 回写池文件,否则文件里
// 永远是最旧的已作废 token —— 重启后换发失败(Kimi 轮换即作废旧票)。
// 旧实现的三个问题(审计+可靠性计划确认):
//  1. 固定名 .tmp:多写者互踩
//  2. 所有错误静默吞掉:只读挂载下"内存轮换成功、文件仍旧值",重启即死
//  3. 无并发保护
//
// 并发模型:单副本部署(路线图裁定不做双活)下,进程内互斥由各 Client 持有
// (kimi 已有 c.mu,glm 于 A2 补齐);跨进程用 O_EXCL 锁文件做轻量保护,
// 拿不到锁的调用方放弃本轮回写并上抛错误(内存轮换已成功,只是文件回写
// 延迟到下次换发,由调用方告警)。
package poolfile

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// ReplaceToken 把池文件中等于 oldToken 的行替换为 newToken(原子写)。
//
// 语义与旧实现一致:跳过空行与 # 注释行;oldToken 与 newToken 都视为命中
// (幂等,防重复追加);均未命中则追加到末尾;保留空行/注释/末尾换行。
//
// 返回 replaced 供调用方区分"替换"与"追加";错误一律上抛(含锁超时),
// 由调用方告警——禁止静默吞错(A2 的核心教训)。
func ReplaceToken(path, oldToken, newToken, tag string) (bool, error) {
	if path == "" || oldToken == "" || newToken == "" {
		return false, nil
	}
	release, err := lock(path+".lock", tag)
	if err != nil {
		return false, fmt.Errorf("获取锁超时(%s.lock,持锁者可能已崩溃残留): %w", path, err)
	}
	defer release()

	data, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("读池文件: %w", err)
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	// 记录并弹出尾部空元素(文件以 \n 结尾时 split 会产生),避免 join 后双换行
	hadTrailing := false
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
		hadTrailing = true
	}
	replaced := false
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if line == oldToken || line == newToken {
			lines[i] = newToken
			replaced = true
		}
	}
	if !replaced {
		lines = append(lines, newToken)
	}
	out := strings.Join(lines, "\n")
	if hadTrailing || !strings.HasSuffix(out, "\n") {
		out += "\n" // 末尾换行(loadTokens 按行读,无尾换行会导致加载异常)
	}

	// 唯一 tmp 名:消除固定名 .tmp 的多写者互踩
	tmp := fmt.Sprintf("%s.tmp.%s.%d", path, tag, os.Getpid())
	if err := os.WriteFile(tmp, []byte(out), 0o644); err != nil {
		return replaced, fmt.Errorf("写临时文件: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp) // 清理半成品
		return replaced, fmt.Errorf("rename: %w", err)
	}
	return replaced, nil
}

// lock 创建 O_EXCL 锁文件;已存在则每 100ms 重试,最长 3s。
// 锁文件 mtime 超过 30s 视为持有者崩溃残留,抢占删除(否则一次崩溃
// 就永久禁用回写)。成功返回释放函数。
func lock(lockPath, tag string) (func(), error) {
	deadline := time.Now().Add(3 * time.Second)
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_, _ = f.WriteString(tag + " " + time.Now().Format(time.RFC3339) + "\n")
			_ = f.Close()
			return func() { _ = os.Remove(lockPath) }, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
		if fi, statErr := os.Stat(lockPath); statErr == nil && time.Since(fi.ModTime()) > 30*time.Second {
			_ = os.Remove(lockPath) // 陈锁抢占
			continue
		}
		if time.Now().After(deadline) {
			return nil, os.ErrExist
		}
		time.Sleep(100 * time.Millisecond)
	}
}
