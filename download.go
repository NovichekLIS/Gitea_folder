// Copyright 2014 The Gogs Authors. All rights reserved.
// Copyright 2018 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import (
    "bytes"
    "fmt"
    "io"
    "net/url"
    "os/exec" 
    "path"
    "strings"
    "time"

    git_model "code.gitea.io/gitea/models/git"
    "code.gitea.io/gitea/modules/git"
    "code.gitea.io/gitea/modules/httpcache"
    "code.gitea.io/gitea/modules/lfs"
    "code.gitea.io/gitea/modules/log"
    "code.gitea.io/gitea/modules/setting"
    "code.gitea.io/gitea/modules/storage"
    "code.gitea.io/gitea/routers/common"
    "code.gitea.io/gitea/services/context"
)

// ServeBlobOrLFS download a git.Blob redirecting to LFS if necessary
func ServeBlobOrLFS(ctx *context.Context, blob *git.Blob, lastModified *time.Time) error {
    if httpcache.HandleGenericETagTimeCache(ctx.Req, ctx.Resp, `"`+blob.ID.String()+`"`, lastModified) {
        return nil
    }

    dataRc, err := blob.DataAsync()
    if err != nil {
        return err
    }
    closed := false
    defer func() {
        if closed {
            return
        }
        if err = dataRc.Close(); err != nil {
            log.Error("ServeBlobOrLFS: Close: %v", err)
        }
    }()

    pointer, _ := lfs.ReadPointer(dataRc)
    if pointer.IsValid() {
        meta, _ := git_model.GetLFSMetaObjectByOid(ctx, ctx.Repo.Repository.ID, pointer.Oid)
        if meta == nil {
            if err = dataRc.Close(); err != nil {
                log.Error("ServeBlobOrLFS: Close: %v", err)
            }
            closed = true
            return common.ServeBlob(ctx.Base, ctx.Repo.Repository, ctx.Repo.TreePath, blob, lastModified)
        }
        if httpcache.HandleGenericETagCache(ctx.Req, ctx.Resp, `"`+pointer.Oid+`"`) {
            return nil
        }

        if setting.LFS.Storage.ServeDirect() {
            // If we have a signed url (S3, object storage, blob storage), redirect to this directly.
            u, err := storage.LFS.URL(pointer.RelativePath(), blob.Name(), ctx.Req.Method, nil)
            if u != nil && err == nil {
                ctx.Redirect(u.String())
                return nil
            }
        }

        lfsDataRc, err := lfs.ReadMetaObject(meta.Pointer)
        if err != nil {
            return err
        }
        defer func() {
            if err = lfsDataRc.Close(); err != nil {
                log.Error("ServeBlobOrLFS: Close: %v", err)
            }
        }()
        common.ServeContentByReadSeeker(ctx.Base, ctx.Repo.TreePath, lastModified, lfsDataRc)
        return nil
    }
    if err = dataRc.Close(); err != nil {
        log.Error("ServeBlobOrLFS: Close: %v", err)
    }
    closed = true

    return common.ServeBlob(ctx.Base, ctx.Repo.Repository, ctx.Repo.TreePath, blob, lastModified)
}

func getBlobForEntry(ctx *context.Context) (*git.Blob, *time.Time) {
    entry, err := ctx.Repo.Commit.GetTreeEntryByPath(ctx.Repo.TreePath)
    if err != nil {
        if git.IsErrNotExist(err) {
            ctx.NotFound(err)
        } else {
            ctx.ServerError("GetTreeEntryByPath", err)
        }
        return nil, nil
    }

    if entry.IsDir() || entry.IsSubModule() {
        ctx.NotFound(nil)
        return nil, nil
    }

    latestCommit, err := ctx.Repo.GitRepo.GetTreePathLatestCommit(ctx.Repo.Commit.ID.String(), ctx.Repo.TreePath)
    if err != nil {
        ctx.ServerError("GetTreePathLatestCommit", err)
        return nil, nil
    }
    lastModified := &latestCommit.Committer.When

    return entry.Blob(), lastModified
}

// SingleDownload download a file by repos path
func SingleDownload(ctx *context.Context) {
    blob, lastModified := getBlobForEntry(ctx)
    if blob == nil {
        return
    }

    if err := common.ServeBlob(ctx.Base, ctx.Repo.Repository, ctx.Repo.TreePath, blob, lastModified); err != nil {
        ctx.ServerError("ServeBlob", err)
    }
}

// SingleDownloadOrLFS download a file by repos path redirecting to LFS if necessary
func SingleDownloadOrLFS(ctx *context.Context) {
    blob, lastModified := getBlobForEntry(ctx)
    if blob == nil {
        return
    }

    if err := ServeBlobOrLFS(ctx, blob, lastModified); err != nil {
        ctx.ServerError("ServeBlobOrLFS", err)
    }
}

// DownloadByID download a file by sha1 ID
func DownloadByID(ctx *context.Context) {
    blob, err := ctx.Repo.GitRepo.GetBlob(ctx.PathParam("sha"))
    if err != nil {
        if git.IsErrNotExist(err) {
            ctx.NotFound(nil)
        } else {
            ctx.ServerError("GetBlob", err)
        }
        return
    }
    if err = common.ServeBlob(ctx.Base, ctx.Repo.Repository, ctx.Repo.TreePath, blob, nil); err != nil {
        ctx.ServerError("ServeBlob", err)
    }
}

// DownloadByIDOrLFS download a file by sha1 ID taking account of LFS
func DownloadByIDOrLFS(ctx *context.Context) {
    blob, err := ctx.Repo.GitRepo.GetBlob(ctx.PathParam("sha"))
    if err != nil {
        if git.IsErrNotExist(err) {
            ctx.NotFound(nil)
        } else {
            ctx.ServerError("GetBlob", err)
        }
        return
    }
    if err = ServeBlobOrLFS(ctx, blob, nil); err != nil {
        ctx.ServerError("ServeBlob", err)
    }
}

func DownloadFolder(ctx *context.Context) {
	// Get path from route parameter
	treePath := ctx.PathParam("*")
	
	// Get branch name from route parameter (if present)
	branchName := ctx.PathParam("branchname")
	
	// Get format from query parameter
	format := ctx.Req.URL.Query().Get("format")
	if format == "" {
		format = "zip" // default
	}
	
	// Если путь не указан, используем текущий путь из контекста
	if treePath == "" && ctx.Repo.TreePath != "" {
		treePath = ctx.Repo.TreePath
	}
	
	// URL decode the path
	decodedPath, err := url.PathUnescape(treePath)
	if err != nil {
		decodedPath = treePath
	}
	
	// Remove leading slash if present
	decodedPath = strings.TrimPrefix(decodedPath, "/")
	
	// Если путь ".", это вся репа, оставляем как есть (git archive обработает)
	if decodedPath == "." {
		decodedPath = ""
	}
	
	// Validate repository access
	if ctx.Repo.Repository == nil || ctx.Repo.GitRepo == nil {
		ctx.NotFound(fmt.Errorf("repository not found"))
		return
	}
	
	// Determine which branch to use
	var targetBranch string
	if branchName != "" {
		// Use branch from URL
		targetBranch = branchName
	} else if ctx.Repo.Commit != nil {
		// Commit exists in context, use default branch
		targetBranch = ctx.Repo.Repository.DefaultBranch
		if targetBranch == "" {
			targetBranch = "main"
		}
	} else {
		// No branch specified, use default
		targetBranch = ctx.Repo.Repository.DefaultBranch
		if targetBranch == "" {
			targetBranch = "main"
		}
	}
	
	// Get commit for the branch
	commit, err := ctx.Repo.GitRepo.GetCommit(targetBranch)
	if err != nil {
		ctx.ServerError("GetCommit", err)
		return
	}
	
	// Verify path exists and is a directory (если путь указан)
	if decodedPath != "" {
		_, err := commit.SubTree(decodedPath)
		if err != nil {
			if git.IsErrNotExist(err) {
				ctx.NotFound(fmt.Errorf("path '%s' not found in branch '%s'", decodedPath, targetBranch))
			} else {
				ctx.ServerError("CheckDirectory", err)
			}
			return
		}
	}
	
	// Set download headers
	folderName := path.Base(decodedPath)
	if folderName == "" || folderName == "." || folderName == "/" {
		folderName = ctx.Repo.Repository.Name
	}
	
	// Determine file extension based on format
	var fileExt string
	switch format {
	case "tar":
		fileExt = "tar"
	case "tar.gz", "tgz":
		fileExt = "tar.gz"
	default: // zip
		fileExt = "zip"
	}
	
	archiveName := fmt.Sprintf("%s-%s.%s", folderName, commit.ID.String()[:7], fileExt)
	
	// Set Content-Type based on format
	switch format {
	case "tar":
		ctx.Resp.Header().Set("Content-Type", "application/x-tar")
	case "tar.gz", "tgz":
		ctx.Resp.Header().Set("Content-Type", "application/gzip")
	default: // zip
		ctx.Resp.Header().Set("Content-Type", "application/zip")
	}
	
	ctx.Resp.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, archiveName))
	
	// Используем git archive для создания архива напрямую в ответ
	err = createGitArchive(ctx.Resp, ctx.Repo.GitRepo.Path, commit.ID.String(), decodedPath, format)
	if err != nil {
		ctx.ServerError("CreateArchive", err)
		return
	}
}

// createGitArchive создает архив через git archive и пишет напрямую в writer
func createGitArchive(w io.Writer, repoPath string, commitHash string, treePath string, format string) error {
	// Определяем аргументы для git archive
	var formatArg string
	switch format {
	case "tar":
		formatArg = "tar"
	case "tar.gz", "tgz":
		formatArg = "tar.gz"
	default: // zip (по умолчанию)
		formatArg = "zip"
	}
	
	// Собираем аргументы команды
	args := []string{"archive", "--format=" + formatArg}
	
	// Добавляем хеш коммита
	args = append(args, commitHash)
	
	// Добавляем путь, если он указан и не равен "." (вся репа)
	if treePath != "" && treePath != "." {
		// Убеждаемся, что путь не начинается с /
		treePath = strings.TrimPrefix(treePath, "/")
		args = append(args, treePath)
	}
	
	// Создаем команду
	cmd := exec.Command("git", args...)
	cmd.Dir = repoPath
	
	// Направляем stdout в writer
	cmd.Stdout = w
	
	// Направляем stderr в буфер для диагностики ошибок
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	
	// Выполняем команду
	if err := cmd.Run(); err != nil {
		// Возвращаем подробную ошибку с выводом stderr
		return fmt.Errorf("git archive failed: %v\nstderr: %s", err, stderr.String())
	}
	
	return nil
}