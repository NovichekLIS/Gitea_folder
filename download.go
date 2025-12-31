// Copyright 2014 The Gogs Authors. All rights reserved.
// Copyright 2018 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import (
    "archive/zip"
    "fmt"
    "io"
    "net/url" 
    "path"
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
    treePath := ctx.PathParam("*")
    if len(treePath) == 0 {
        ctx.NotFound(nil)
        return
    }
    
    // URL decode the path if needed
    decodedPath, err := url.PathUnescape(treePath)
    if err != nil {
        decodedPath = treePath
    }
    
    log.Info("DownloadFolder: treePath=%q, decodedPath=%q", treePath, decodedPath)
    
    // Get the commit - try to use the commit from context first
    var commit *git.Commit
    
    // Check if we have a commit in context (this should be set by RepoAssignment middleware)
    if ctx.Repo.Commit != nil {
        commit = ctx.Repo.Commit
        log.Info("DownloadFolder: Using existing commit from context: %s", commit.ID.String())
    } else {
        // No commit in context, try to get it from git repo
        // First try to get the current branch
        headRef, err := ctx.Repo.GitRepo.GetHEADBranch()
        if err != nil {
            log.Error("Failed to get HEAD branch: %v", err)
            // Use default branch as fallback
            ref := ctx.Repo.Repository.DefaultBranch
            log.Info("DownloadFolder: Using default branch: %s", ref)
            commit, err = ctx.Repo.GitRepo.GetCommit(ref)
            if err != nil {
                ctx.ServerError("GetCommit", err)
                return
            }
        } else {
            // Use the HEAD branch
            log.Info("DownloadFolder: Using HEAD branch: %s", headRef.Name)
            commit, err = ctx.Repo.GitRepo.GetCommit(headRef.Name)
            if err != nil {
                // Fallback to default branch
                ref := ctx.Repo.Repository.DefaultBranch
                log.Info("DownloadFolder: Failed to get commit for HEAD, using default branch: %s", ref)
                commit, err = ctx.Repo.GitRepo.GetCommit(ref)
                if err != nil {
                    ctx.ServerError("GetCommit", err)
                    return
                }
            }
        }
    }
    
    if commit == nil {
        log.Error("DownloadFolder: Commit is nil")
        ctx.NotFound(fmt.Errorf("commit not found"))
        return
    }
    
    log.Info("DownloadFolder: Using commit %s for path %s", commit.ID.String(), decodedPath)

    // Verify it's a directory
    entry, err := commit.GetTreeEntryByPath(decodedPath)
    if err != nil {
        log.Error("GetTreeEntryByPath failed for %q in commit %s: %v", decodedPath, commit.ID.String(), err)
        
        if git.IsErrNotExist(err) {
            ctx.NotFound(fmt.Errorf("path not found: %s", decodedPath))
        } else {
            ctx.ServerError("GetTreeEntryByPath", err)
        }
        return
    }

    if !entry.IsDir() {
        ctx.NotFound(fmt.Errorf("path is not a directory: %s", decodedPath))
        return
    }

    log.Info("DownloadFolder: Found directory entry: %s", entry.Name())

    // Set download headers
    folderName := path.Base(decodedPath)
    if folderName == "" || folderName == "." || folderName == "/" {
        folderName = ctx.Repo.Repository.Name
    }
    archiveName := fmt.Sprintf("%s-%s.zip", folderName, commit.ID.String()[:7])
    ctx.Resp.Header().Set("Content-Type", "application/zip")
    ctx.Resp.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, archiveName))

    // Create ZIP archive
    zipWriter := zip.NewWriter(ctx.Resp)
    defer zipWriter.Close()

    // Recursively add folder contents to ZIP
    err = addFolderToZip(zipWriter, commit, decodedPath, "")
    if err != nil {
        log.Error("Failed to create zip archive: %v", err)
        ctx.ServerError("CreateZip", err)
        return
    }
    
    log.Info("DownloadFolder: Successfully created zip for %q", decodedPath)
}

// addFolderToZip recursively adds folder contents to ZIP archive
func addFolderToZip(zipWriter *zip.Writer, commit *git.Commit, treePath string, zipPath string) error {
    // Get the tree for this path
    tree, err := commit.SubTree(treePath)
    if err != nil {
        return err
    }

    // List all entries in the tree
    entries, err := tree.ListEntries()
    if err != nil {
        return err
    }

    for _, entry := range entries {
        fullPath := path.Join(treePath, entry.Name())
        zipEntryPath := path.Join(zipPath, entry.Name())
        
        if entry.IsDir() {
            // Recursively process subdirectories
            err = addFolderToZip(zipWriter, commit, fullPath, zipEntryPath)
            if err != nil {
                return err
            }
        } else {
            // Add file to archive
            blob := entry.Blob()
            dataReader, err := blob.DataAsync()
            if err != nil {
                return err
            }
            defer dataReader.Close()
            
            zipEntry, err := zipWriter.Create(zipEntryPath)
            if err != nil {
                return err
            }
            
            _, err = io.Copy(zipEntry, dataReader)
            if err != nil {
                return err
            }
        }
    }
    return nil
}