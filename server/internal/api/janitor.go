package api

import (
	"context"
	"log"
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

// CleanupStats 是一次清理的结果。
type CleanupStats struct {
	QueueItems  int64
	Attachments int
	TempFiles   int
}

// Cleanup 删除入队超过 30 天的队列项、90 天未被访问且未被头像/朋友圈引用的附件（元数据与文件），
// 以及进程崩溃后残留的上传临时文件。
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
	dir := filepath.Join(dataDir, "attachments")
	cs.TempFiles = sweepTempFiles(dir, now)
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
			err := os.Rename(filepath.Join(dir, id), filepath.Join(dir, trashPrefix+id))
			if err == nil || os.IsNotExist(err) { // 文件本来就不在：只剩元数据，照样删
				moved = append(moved, id)
				continue
			}
			log.Printf("[janitor] 移动附件文件 %s 失败，本轮跳过: %v", maskID(id), err)
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
				log.Printf("[janitor] 删除附件文件 %s 失败（留待之后清理）: %v", maskID(id), err)
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
			log.Printf("[janitor] 恢复附件文件 %s 失败: %v", maskID(id), err)
		}
	}
}

// sweepTempFiles 删除过期的上传临时文件（进程在上传中途崩溃时残留）与删除失败残留的 .trash- 文件，返回删除个数。
func sweepTempFiles(dir string, now time.Time) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[janitor] 读取附件目录失败: %v", err)
		}
		return 0
	}
	n := 0
	for _, e := range entries {
		var ttl time.Duration
		switch name := e.Name(); {
		case strings.HasPrefix(name, uploadTempPrefix):
			ttl = uploadTempTTL
		case strings.HasPrefix(name, trashPrefix):
			ttl = trashTTL
		default:
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) < ttl {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			log.Printf("[janitor] 删除临时文件失败: %v", err)
			continue
		}
		n++
	}
	return n
}

// RunJanitor 立即清理一次，之后每小时一次，直到 ctx 结束。
func RunJanitor(ctx context.Context, st *store.Store, dataDir string) {
	tick := time.NewTicker(JanitorInterval)
	defer tick.Stop()
	for {
		cs, err := Cleanup(ctx, st, dataDir, time.Now())
		if err != nil {
			log.Printf("[janitor] 清理失败: %v", err)
		} else if cs.QueueItems > 0 || cs.Attachments > 0 || cs.TempFiles > 0 {
			log.Printf("[janitor] 清理过期队列项 %d 条、闲置附件 %d 个、临时文件 %d 个", cs.QueueItems, cs.Attachments, cs.TempFiles)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
