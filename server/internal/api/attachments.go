package api

import (
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"hush/server/internal/store"
)

// MaxAttachmentSize 是默认附件大小上限（100MB）。
const MaxAttachmentSize = 100 << 20

// POST /v1/attachments（原始字节）→ {id, size}
func (s *Server) handleUploadAttachment(w http.ResponseWriter, r *http.Request, uid string) {
	limit := s.cfg.MaxAttachmentBytes
	if r.ContentLength > limit {
		fail(w, codeTooLarge, "附件不能超过 100MB")
		return
	}
	// 每用户 24 小时上传总量配额：先按声明的长度预占额度（并发上传彼此可见），读完后按实际大小再核一次。
	res, err := s.uploads.reserve(r.Context(), s, uid, max(r.ContentLength, 1))
	if err != nil {
		internalErr(w, r, err)
		return
	}
	if res == nil {
		fail(w, codeRateLimited, "24 小时内上传的附件总量已达上限")
		return
	}
	defer res.release()
	// 大文件上传：放宽本请求的读写超时
	extendDeadlines(w, true, true, s.cfg.BlobTimeout)
	if err := os.MkdirAll(s.attachmentDir, 0o700); err != nil {
		internalErr(w, r, err)
		return
	}
	tmp, err := os.CreateTemp(s.attachmentDir, uploadTempPrefix+"*")
	if err != nil {
		internalErr(w, r, err)
		return
	}
	defer os.Remove(tmp.Name()) // 成功时已被 rename，删除会无害地失败
	body := http.MaxBytesReader(w, r.Body, limit)
	size, err := io.Copy(tmp, body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			fail(w, codeTooLarge, "附件不能超过 100MB")
			return
		}
		fail(w, codeBadRequest, "读取上传内容失败")
		return
	}
	if size == 0 {
		fail(w, codeBadRequest, "附件内容为空")
		return
	}
	// 按实际大小重新核对（未声明 Content-Length 的分块上传只预占了 1 字节）
	if fits, err := res.resize(r.Context(), size); err != nil {
		internalErr(w, r, err)
		return
	} else if !fits {
		fail(w, codeRateLimited, "24 小时内上传的附件总量已达上限")
		return
	}
	id := store.NewID()
	if err := os.Rename(tmp.Name(), filepath.Join(s.attachmentDir, id)); err != nil {
		internalErr(w, r, err)
		return
	}
	if err := s.st.CreateAttachment(r.Context(), store.Attachment{ID: id, UploaderID: uid, Size: size, CreatedAt: store.NowMillis()}); err != nil {
		os.Remove(filepath.Join(s.attachmentDir, id))
		internalErr(w, r, err)
		return
	}
	log.Printf("[attach] user=%s 上传 %s（%d 字节）", uid, maskID(id), size)
	ok(w, obj{"id": id, "size": size})
}

// GET /v1/attachments/{id} → 原始字节（支持 Range）
func (s *Server) handleDownloadAttachment(w http.ResponseWriter, r *http.Request, uid string) {
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		fail(w, codeNotFound, "附件不存在")
		return
	}
	if _, err := s.st.GetAttachment(r.Context(), id); err != nil {
		storeErr(w, r, err, "附件不存在")
		return
	}
	f, err := os.Open(filepath.Join(s.attachmentDir, id))
	if err != nil {
		if os.IsNotExist(err) {
			fail(w, codeNotFound, "附件不存在")
			return
		}
		internalErr(w, r, err)
		return
	}
	defer f.Close()
	if err := s.st.TouchAttachment(r.Context(), id, store.NowMillis()); err != nil {
		log.Printf("[attach] 更新访问时间失败 %s: %v", maskID(id), err)
	}
	extendDeadlines(w, false, true, s.cfg.BlobTimeout)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	http.ServeContent(w, r, "", time.Time{}, f)
}
