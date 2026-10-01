package api

import (
	"context"
	"hush/server/internal/logx"
	"os"
	"path/filepath"
	"strings"
	"time"

	"hush/server/internal/store"
)

// 数据保留期。
const (
	QueueTTL          = 30 * 24 * time.Hour
	AttachmentIdleTTL = 90 * 24 * time.Hour
	JanitorInterval   = time.Hour
	cleanupBatch      = 500

	uploadTempPrefix = ".upload-" // 上传中的临时文件（成功后改名为附件 id）
	trashPrefix      = ".trash-"  // 清理时先改名到这里，确认元数据已删除后再删文件
	uploadTempTTL    = 24 * time.Hour
	trashTTL         = time.Hour
)

// 其余只增不减的数据的保留期。
const (
	NotificationTTL   = 90 * 24 * time.Hour // 朋友圈互动通知
	HandledRequestTTL = 30 * 24 * time.Hour // 已接受 / 已拒绝的好友申请
	orphanFileTTL     = time.Hour           // 没有元数据的附件文件（上传在「改名之后、写库之前」崩溃）
)

// CleanupStats 是一次清理的结果。
type CleanupStats struct {
	QueueItems  int64
	Attachments int
	TempFiles   int
	Other       int64 // 验证码与失败计数、幂等键、旧通知、已处理的好友申请、无人引用的共享正文
}

// MoveToTrash 把附件文件移进回收站（.trash-<id>）。先把 mtime 改成现在再改名：rename 不改 mtime，
// 不这样做的话一个 90 天没动过的文件一进回收站就「早已过了保留期」，保留期形同虚设。
func MoveToTrash(dir, id string, now time.Time) error {
	path := filepath.Join(dir, id)
	if err := os.Chtimes(path, now, now); err != nil {
		return err
	}
	return os.Rename(path, filepath.Join(dir, trashPrefix+id))
}

// Cleanup 删除入队超过 30 天的队列项、90 天未被访问且未被头像/朋友圈引用的附件（元数据与文件）、
// 进程崩溃后残留的上传临时文件与孤儿文件，以及其他只增不减的表里的过期数据。删除都是分批的小事务。
//
// 附件的删除顺序：先把文件改名到 .trash-<id>，再用带同样引用条件的一条 DELETE 删除元数据，
// 只有确实删掉元数据的才删除文件，其余（期间被重新引用、或文件改名失败的）改回原名、保留元数据，下次再试。
// 这样既不会出现「元数据已删、文件删除失败后永远成为孤儿」，也不会删掉刚被设为头像的附件。
func Cleanup(ctx context.Context, st *store.Store, dataDir string, now time.Time) (CleanupStats, error) {
	var cs CleanupStats
	n, err := st.PruneQueue(ctx, now.Add(-QueueTTL).UnixMilli())
	if err != nil {
		return cs, err
	}
	cs.QueueItems = n
	for _, prune := range []func() (int64, error){
		func() (int64, error) { return st.PruneAuth(ctx, now.UnixMilli()) },
		func() (int64, error) { return st.PruneSendDedup(ctx, now.Add(-store.SendDedupTTL).UnixMilli()) },
		func() (int64, error) { return st.PruneNotifications(ctx, now.Add(-NotificationTTL).UnixMilli()) },
		func() (int64, error) { return st.PruneHandledRequests(ctx, now.Add(-HandledRequestTTL).UnixMilli()) },
		func() (int64, error) { return st.PruneOrphanEnvelopes(ctx, now.Add(-time.Hour).UnixMilli()) },
	} {
		n, err := prune()
		if err != nil {
			return cs, err
		}
		cs.Other += n
	}
	dir := filepath.Join(dataDir, "attachments")
	cs.TempFiles = sweepTempFiles(ctx, st, dir, now)
	before := now.Add(-AttachmentIdleTTL).UnixMilli()
	for {
		ids, err := st.StaleAttachments(ctx, before, cleanupBatch)
		if err != nil {
			return cs, err
		}
		if len(ids) == 0 {
			break
		}
		var moved []string
		for _, id := range ids {
			err := MoveToTrash(dir, id, now)
			if err == nil || os.IsNotExist(err) { // 文件本来就不在：只剩元数据，照样删
				moved = append(moved, id)
				continue
			}
			logx.Warnf("[janitor] 移动附件文件 %s 失败，本轮跳过: %v", maskID(id), err)
		}
		deleted, err := st.DeleteStaleAttachments(ctx, moved, before)
		if err != nil {
			restoreTrash(dir, moved)
			return cs, err
		}
		var keep []string
		for _, id := range moved {
			if !deleted[id] {
				keep = append(keep, id)
				continue
			}
			if err := os.Remove(filepath.Join(dir, trashPrefix+id)); err != nil && !os.IsNotExist(err) {
				logx.Warnf("[janitor] 删除附件文件 %s 失败（留待之后清理）: %v", maskID(id), err)
			}
		}
		restoreTrash(dir, keep)
		cs.Attachments += len(deleted)
		if len(deleted) == 0 || len(ids) < cleanupBatch {
			break // 没有进展（例如文件都改名失败）时不再重试，下次定时清理再来
		}
	}
	return cs, nil
}

// restoreTrash 把 .trash-<id> 改回 <id>（元数据未删除的附件）。
func restoreTrash(dir string, ids []string) {
	for _, id := range ids {
		if err := os.Rename(filepath.Join(dir, trashPrefix+id), filepath.Join(dir, id)); err != nil && !os.IsNotExist(err) {
			logx.Warnf("[janitor] 恢复附件文件 %s 失败: %v", maskID(id), err)
		}
	}
}

// sweepTempFiles 扫一遍附件目录，返回删除的文件数：
//   - 过期的上传临时文件（进程在上传中途崩溃时残留）；
//   - 回收站里的文件：元数据还在的（清理任务在「改名之后、删元数据之前」崩溃）改回原名；元数据已删的过了保留期才删除；
//   - 孤儿文件：名字是附件 id、但没有元数据（上传在「改名之后、写库之前」崩溃），超过 1 小时删除。
func sweepTempFiles(ctx context.Context, st *store.Store, dir string, now time.Time) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			logx.Warnf("[janitor] 读取附件目录失败: %v", err)
		}
		return 0
	}
	n := 0
	remove := func(name string) {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			logx.Warnf("[janitor] 删除文件失败: %v", err)
			return
		}
		n++
	}
	age := func(e os.DirEntry) (time.Duration, bool) {
		info, err := e.Info()
		if err != nil {
			return 0, false
		}
		return now.Sub(info.ModTime()), true
	}
	var trash, plain []os.DirEntry
	for _, e := range entries {
		switch name := e.Name(); {
		case strings.HasPrefix(name, uploadTempPrefix):
			if a, ok := age(e); ok && a >= uploadTempTTL {
				remove(name)
			}
		case strings.HasPrefix(name, trashPrefix):
			trash = append(trash, e)
		case len(name) == 36 && e.Type().IsRegular():
			if a, ok := age(e); ok && a >= orphanFileTTL {
				plain = append(plain, e) // 够旧的才查：刚改名的上传可能还没来得及写元数据
			}
		}
	}
	ids := make([]string, 0, len(trash)+len(plain))
	for _, e := range trash {
		ids = append(ids, strings.TrimPrefix(e.Name(), trashPrefix))
	}
	for _, e := range plain {
		ids = append(ids, e.Name())
	}
	if len(ids) == 0 {
		return n
	}
	known, err := st.ExistingAttachments(ctx, ids)
	if err != nil {
		logx.Warnf("[janitor] 查询附件元数据失败: %v", err)
		return n
	}
	for _, e := range trash {
		id := strings.TrimPrefix(e.Name(), trashPrefix)
		if known[id] {
			restoreTrash(dir, []string{id})
			continue
		}
		if a, ok := age(e); ok && a >= trashTTL {
			remove(e.Name())
		}
	}
	for _, e := range plain {
		if !known[e.Name()] {
			remove(e.Name())
		}
	}
	return n
}

// RunJanitor 立即清理一次，之后每小时一次，直到 ctx 结束。extra 是每轮额外执行的清理（如 Hub 的 epoch 表）。
func RunJanitor(ctx context.Context, st *store.Store, dataDir string, extra ...func(now time.Time)) {
	tick := time.NewTicker(JanitorInterval)
	defer tick.Stop()
	for {
		now := time.Now()
		cs, err := Cleanup(ctx, st, dataDir, now)
		if err != nil {
			if ctx.Err() == nil {
				logx.Errorf("[janitor] 清理失败: %v", err)
			}
		} else if cs.QueueItems > 0 || cs.Attachments > 0 || cs.TempFiles > 0 || cs.Other > 0 {
			logx.Infof("[janitor] 清理过期队列项 %d 条、闲置附件 %d 个、临时 / 孤儿文件 %d 个、其他过期记录 %d 条", cs.QueueItems, cs.Attachments, cs.TempFiles, cs.Other)
		}
		for _, fn := range extra {
			fn(now)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
