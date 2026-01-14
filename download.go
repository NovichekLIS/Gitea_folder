// Copyright 2014 The Gogs Authors. All rights reserved.
// Copyright 2018 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import (
    "archive/tar"
    "archive/zip"
    "compress/gzip"
    "fmt"
    "io"
    "net/url" 
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

// DownloadFolder download a folder as archive in specified format
func DownloadFolder(ctx *context.Context) {
    // Получаем полный URL путь
    fullPath := ctx.Req.URL.Path
    log.Info("DownloadFolder START: fullPath=%q", fullPath)
    
    // Получаем путь из параметра маршрута
    treePath := ctx.PathParam("*")
    log.Info("DownloadFolder: treePath from param='%s'", treePath)
    
    // Получаем формат из query параметра
    format := ctx.Req.URL.Query().Get("format")
    if format == "" {
        format = "zip" // по умолчанию
    }
    log.Info("DownloadFolder: format=%q", format)
    
    // Если путь пустой, используем текущий путь из контекста
    if treePath == "" {
        // Пробуем получить путь из контекста репозитория
        if ctx.Repo.TreePath != "" {
            treePath = ctx.Repo.TreePath
            log.Info("DownloadFolder: using TreePath from context='%s'", treePath)
        } else {
            treePath = "."
            log.Info("DownloadFolder: using default path='.'")
        }
    }
    
    log.Info("DownloadFolder: raw treePath=%q", treePath)
    
    // URL decode the path
    decodedPath, err := url.PathUnescape(treePath)
    if err != nil {
        decodedPath = treePath
    }
    
    // Удаляем начальный слэш если есть
    decodedPath = strings.TrimPrefix(decodedPath, "/")
    
    // Если путь ".", значит хотим скачать весь репозиторий
    if decodedPath == "." {
        decodedPath = ""
    }
    
    log.Info("DownloadFolder: decodedPath=%q", decodedPath)
    
    // ВАЖНО: Проверяем, что у нас есть репозиторий
    if ctx.Repo.Repository == nil {
        log.Error("DownloadFolder: Repository is nil")
        ctx.NotFound(fmt.Errorf("repository not found"))
        return
    }
    
    if ctx.Repo.GitRepo == nil {
        log.Error("DownloadFolder: GitRepo is nil")
        ctx.NotFound(fmt.Errorf("git repository not found"))
        return
    }
    
    log.Info("DownloadFolder: Repository name=%s", ctx.Repo.Repository.Name)
    log.Info("DownloadFolder: Repository default branch=%s", ctx.Repo.Repository.DefaultBranch)
    
    // Получаем коммит
    var commit *git.Commit
    if ctx.Repo.Commit != nil {
        commit = ctx.Repo.Commit
        log.Info("DownloadFolder: Using commit from context: %s", commit.ID.String())
    } else {
        // Получаем текущий коммит из ветки по умолчанию
        ref := ctx.Repo.Repository.DefaultBranch
        if ref == "" {
            ref = "main"
        }
        log.Info("DownloadFolder: No commit in context, using default branch: %s", ref)
        
        commit, err = ctx.Repo.GitRepo.GetCommit(ref)
        if err != nil {
            log.Error("DownloadFolder: Failed to get commit for default branch %s: %v", ref, err)
            ctx.ServerError("GetCommit", err)
            return
        }
        
        // Сохраняем коммит в контексте для будущих вызовов
        ctx.Repo.Commit = commit
    }
    
    if commit == nil {
        log.Error("DownloadFolder: Commit is nil after all attempts")
        ctx.NotFound(fmt.Errorf("commit not found"))
        return
    }
    
    log.Info("DownloadFolder: Using commit %s for path %s", commit.ID.String(), decodedPath)
    
    // Для отладки: получаем список всех файлов в корне
    entries, listErr := commit.Tree.ListEntries()
    if listErr == nil {
        var availablePaths []string
        for _, e := range entries {
            availablePaths = append(availablePaths, e.Name())
        }
        log.Info("DownloadFolder: Available paths in root (%d items): %v", len(availablePaths), availablePaths)
    } else {
        log.Error("DownloadFolder: Failed to list entries: %v", listErr)
    }

    // Для пустого пути (скачивание всего репозитории) проверяем, что коммит существует
    if decodedPath != "" {
        log.Info("DownloadFolder: Checking if path '%s' exists...", decodedPath)
        
        // Пробуем получить поддерево
        _, err := commit.SubTree(decodedPath)
        if err != nil {
            log.Error("SubTree failed for %q in commit %s: %v", decodedPath, commit.ID.String(), err)
            
            // Также пробуем GetTreeEntryByPath для более точной ошибки
            entry, entryErr := commit.GetTreeEntryByPath(decodedPath)
            if entryErr == nil {
                log.Info("DownloadFolder: Found entry via GetTreeEntryByPath: %s (IsDir: %v)", entry.Name(), entry.IsDir())
                if !entry.IsDir() {
                    ctx.NotFound(fmt.Errorf("path '%s' is not a directory (it's a file)", decodedPath))
                    return
                }
            } else {
                log.Error("GetTreeEntryByPath also failed: %v", entryErr)
            }
            
            if git.IsErrNotExist(err) {
                ctx.NotFound(fmt.Errorf("path '%s' not found in commit %s", decodedPath, commit.ID.String()[:7]))
            } else {
                ctx.ServerError("CheckDirectory", err)
            }
            return
        }
        
        log.Info("DownloadFolder: Path %s is a valid directory (verified via SubTree)", decodedPath)
    } else {
        log.Info("DownloadFolder: Downloading entire repository")
    }

    // Set download headers
    folderName := path.Base(decodedPath)
    if folderName == "" || folderName == "." || folderName == "/" {
        folderName = ctx.Repo.Repository.Name
    }
    
    log.Info("DownloadFolder: folderName=%q", folderName)
    
    // Определяем расширение файла в зависимости от формата
    var fileExt string
    switch format {
    case "tar":
        fileExt = "tar"
    case "tar.gz":
        fileExt = "tar.gz"
    default: // zip
        fileExt = "zip"
    }
    
    archiveName := fmt.Sprintf("%s-%s.%s", folderName, commit.ID.String()[:7], fileExt)
    
    log.Info("DownloadFolder: archiveName=%q", archiveName)
    
    // Устанавливаем Content-Type в зависимости от формата
    switch format {
    case "tar":
        ctx.Resp.Header().Set("Content-Type", "application/x-tar")
    case "tar.gz":
        ctx.Resp.Header().Set("Content-Type", "application/gzip")
    default: // zip
        ctx.Resp.Header().Set("Content-Type", "application/zip")
    }
    
    ctx.Resp.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, archiveName))

    // Создаем архив в зависимости от формата
    log.Info("DownloadFolder: Creating %s archive...", format)
    switch format {
    case "tar":
        err = createTarArchive(ctx.Resp, commit, decodedPath, false)
    case "tar.gz":
        err = createTarArchive(ctx.Resp, commit, decodedPath, true)
    default: // zip
        err = createZipArchive(ctx.Resp, commit, decodedPath)
    }
    
    if err != nil {
        log.Error("Failed to create %s archive: %v", format, err)
        ctx.ServerError("CreateArchive", err)
        return
    }
    
    log.Info("DownloadFolder: Successfully created %s archive for %q", format, decodedPath)
}

// normalizeFormat нормализует название формата
func normalizeFormat(format string) string {
    format = strings.ToLower(format)
    switch format {
    case "tgz":
        return "tar.gz"
    case "gz", "gzip":
        return "tar.gz"
    default:
        return format
    }
}

// createZipArchive создает ZIP архив
func createZipArchive(w io.Writer, commit *git.Commit, treePath string) error {
    zipWriter := zip.NewWriter(w)
    defer zipWriter.Close()

    return addFolderToZip(zipWriter, commit, treePath, "")
}

// createTarArchive создает TAR архив с опциональным gzip сжатием
func createTarArchive(w io.Writer, commit *git.Commit, treePath string, useGzip bool) error {
    var out io.Writer = w
    
    // Применяем gzip сжатие если нужно
    if useGzip {
        gzipWriter := gzip.NewWriter(w)
        defer gzipWriter.Close()
        out = gzipWriter
    }
    
    tarWriter := tar.NewWriter(out)
    defer tarWriter.Close()
    
    return addFolderToTar(tarWriter, commit, treePath, "")
}

// addFolderToZip рекурсивно добавляет содержимое папки в ZIP архив
func addFolderToZip(zipWriter *zip.Writer, commit *git.Commit, treePath string, zipPath string) error {
    var entries []*git.TreeEntry
    var err error
    
    if treePath == "" {
        entries, err = commit.Tree.ListEntries()
    } else {
        tree, err := commit.SubTree(treePath)
        if err != nil {
            return err
        }
        entries, err = tree.ListEntries()
    }
    
    if err != nil {
        return err
    }

    for _, entry := range entries {
        var fullPath string
        if treePath == "" {
            fullPath = entry.Name()
        } else {
            fullPath = path.Join(treePath, entry.Name())
        }
        
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

// addFolderToTar рекурсивно добавляет содержимое папки в TAR архив
func addFolderToTar(tarWriter *tar.Writer, commit *git.Commit, treePath string, tarPath string) error {
    var entries []*git.TreeEntry
    var err error
    
    if treePath == "" {
        entries, err = commit.Tree.ListEntries()
    } else {
        tree, err := commit.SubTree(treePath)
        if err != nil {
            return err
        }
        entries, err = tree.ListEntries()
    }
    
    if err != nil {
        return err
    }

    for _, entry := range entries {
        var fullPath string
        if treePath == "" {
            fullPath = entry.Name()
        } else {
            fullPath = path.Join(treePath, entry.Name())
        }
        
        tarEntryPath := path.Join(tarPath, entry.Name())
        
        if entry.IsDir() {
            // Создаем запись для директории
            header := &tar.Header{
                Name:     tarEntryPath + "/",
                Mode:     0755,
                Typeflag: tar.TypeDir,
            }
            if err := tarWriter.WriteHeader(header); err != nil {
                return err
            }
            
            // Recursively process subdirectories
            err = addFolderToTar(tarWriter, commit, fullPath, tarEntryPath)
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
            
            // Получаем размер файла
            size := blob.Size()
            
            // Создаем заголовок для файла
            header := &tar.Header{
                Name: tarEntryPath,
                Mode: 0644,
                Size: size,
            }
            
            if err := tarWriter.WriteHeader(header); err != nil {
                return err
            }
            
            // Копируем содержимое файла
            _, err = io.Copy(tarWriter, dataReader)
            if err != nil {
                return err
            }
        }
    }
    return nil
}