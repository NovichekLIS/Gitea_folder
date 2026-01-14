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
    // Получаем путь из параметра маршрута
    treePath := ctx.PathParam("*")
    
    // Получаем формат из query параметра
    format := ctx.Req.URL.Query().Get("format")
    if format == "" {
        format = "zip" // по умолчанию
    }
    
    log.Info("DownloadFolder: path=%q, format=%q", treePath, format)
    
    // Если путь пустой, используем текущий путь из контекста
    if treePath == "" && ctx.Repo.TreePath != "" {
        treePath = ctx.Repo.TreePath
    }
    if treePath == "" {
        treePath = "."
    }
    
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
    if ctx.Repo.Repository == nil || ctx.Repo.GitRepo == nil {
        ctx.NotFound(fmt.Errorf("repository not found"))
        return
    }
    
    // Получаем коммит
    var commit *git.Commit
    var branchName string
    
    // Способ 1: Коммит из контекста (если middleware установил)
    if ctx.Repo.Commit != nil {
        commit = ctx.Repo.Commit
        log.Info("DownloadFolder: Using commit from context: %s", commit.ID.String())
        
        // Пытаемся определить ветку из коммита
        // Это может быть сложно, но попробуем
        branchName = "current" // заглушка
    } else {
        // Способ 2: Получаем текущую ветку из URL или контекста
        
        // Пробуем получить ветку из URL (если есть в пути)
        fullPath := ctx.Req.URL.Path
        branchName = extractBranchFromURL(fullPath)
        
        if branchName == "" {
            // Используем ветку по умолчанию
            branchName = ctx.Repo.Repository.DefaultBranch
            if branchName == "" {
                branchName = "main"
            }
            log.Info("DownloadFolder: Using default branch: %s", branchName)
        } else {
            log.Info("DownloadFolder: Using branch from URL: %s", branchName)
        }
        
        // Получаем коммит для ветки
        var err error
        commit, err = ctx.Repo.GitRepo.GetCommit(branchName)
        if err != nil {
            log.Error("DownloadFolder: Failed to get commit for %s: %v", branchName, err)
            ctx.ServerError("GetCommit", err)
            return
        }
    }
    
    log.Info("DownloadFolder: Using commit %s (branch: %s)", commit.ID.String(), branchName)
    
    // Для отладки: получаем список файлов в корне
    entries, listErr := commit.Tree.ListEntries()
    if listErr == nil {
        var availablePaths []string
        for _, e := range entries {
            availablePaths = append(availablePaths, e.Name())
        }
        log.Info("DownloadFolder: Available in root: %v", availablePaths)
    }
    
    // Проверяем существование пути
    if decodedPath != "" {
        _, err := commit.SubTree(decodedPath)
        if err != nil {
            if git.IsErrNotExist(err) {
                ctx.NotFound(fmt.Errorf("path '%s' not found in branch '%s'", decodedPath, branchName))
            } else {
                ctx.ServerError("CheckDirectory", err)
            }
            return
        }
        log.Info("DownloadFolder: Path %s exists", decodedPath)
    }
    
    // Set download headers
    folderName := path.Base(decodedPath)
    if folderName == "" || folderName == "." || folderName == "/" {
        folderName = ctx.Repo.Repository.Name
    }
    
    // Определяем расширение файла
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
    
    // Устанавливаем Content-Type
    switch format {
    case "tar":
        ctx.Resp.Header().Set("Content-Type", "application/x-tar")
    case "tar.gz":
        ctx.Resp.Header().Set("Content-Type", "application/gzip")
    default: // zip
        ctx.Resp.Header().Set("Content-Type", "application/zip")
    }
    
    ctx.Resp.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, archiveName))
    
    // Создаем архив
    switch format {
    case "tar":
        err = createTarArchive(ctx.Resp, commit, decodedPath, false)
    case "tar.gz":
        err = createTarArchive(ctx.Resp, commit, decodedPath, true)
    default: // zip
        err = createZipArchive(ctx.Resp, commit, decodedPath)
    }
    
    if err != nil {
        log.Error("Failed to create archive: %v", err)
        ctx.ServerError("CreateArchive", err)
        return
    }
    
    log.Info("DownloadFolder: Successfully created %s archive for %s", format, decodedPath)
}

// extractBranchFromURL пытается извлечь имя ветки из URL
func extractBranchFromURL(urlPath string) string {
    // Разбиваем URL на части
    parts := strings.Split(urlPath, "/")
    
    // Ищем паттерны, которые могут указывать на ветку
    // Примеры:
    // /username/repo/src/branch/branch-name/...
    // /username/repo/tree/branch-name/...
    
    for i := 0; i < len(parts)-2; i++ {
        if parts[i] == "src" && parts[i+1] == "branch" && i+2 < len(parts) {
            // Нашли src/branch/branch-name
            return parts[i+2]
        }
    }
    
    for i := 0; i < len(parts)-1; i++ {
        if parts[i] == "tree" && i+1 < len(parts) {
            // Нашли tree/branch-name
            return parts[i+1]
        }
    }
    
    // Не нашли ветку в URL
    return ""
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