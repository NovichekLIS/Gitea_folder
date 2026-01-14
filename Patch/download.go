--- a/routers/web/repo/download.go
+++ b/routers/web/repo/download.go
@@ -XXX,XXX +XXX,XXX @@
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
 
+// DownloadFolder download a folder as archive in specified format
+func DownloadFolder(ctx *context.Context) {
+    // Get path from route parameter
+    treePath := ctx.PathParam("*")
+    
+    // Get branch name from route parameter (if present)
+    branchName := ctx.PathParam("branchname")
+    
+    // Get format from query parameter
+    format := ctx.Req.URL.Query().Get("format")
+    if format == "" {
+        format = "zip" // default
+    }
+    
+    // If path is empty, use current path from context
+    if treePath == "" && ctx.Repo.TreePath != "" {
+        treePath = ctx.Repo.TreePath
+    }
+    if treePath == "" {
+        treePath = "."
+    }
+    
+    // URL decode the path
+    decodedPath, err := url.PathUnescape(treePath)
+    if err != nil {
+        decodedPath = treePath
+    }
+    
+    // Remove leading slash if present
+    decodedPath = strings.TrimPrefix(decodedPath, "/")
+    
+    // If path is ".", download entire repository
+    if decodedPath == "." {
+        decodedPath = ""
+    }
+    
+    // Validate repository access
+    if ctx.Repo.Repository == nil || ctx.Repo.GitRepo == nil {
+        ctx.NotFound(fmt.Errorf("repository not found"))
+        return
+    }
+    
+    // Determine which branch to use
+    var targetBranch string
+    if branchName != "" {
+        // Use branch from URL
+        targetBranch = branchName
+    } else if ctx.Repo.Commit != nil {
+        // Commit exists in context, use default branch
+        targetBranch = ctx.Repo.Repository.DefaultBranch
+        if targetBranch == "" {
+            targetBranch = "main"
+        }
+    } else {
+        // No branch specified, use default
+        targetBranch = ctx.Repo.Repository.DefaultBranch
+        if targetBranch == "" {
+            targetBranch = "main"
+        }
+    }
+    
+    // Get commit for the branch
+    commit, err := ctx.Repo.GitRepo.GetCommit(targetBranch)
+    if err != nil {
+        ctx.ServerError("GetCommit", err)
+        return
+    }
+    
+    // Verify path exists and is a directory
+    if decodedPath != "" {
+        _, err := commit.SubTree(decodedPath)
+        if err != nil {
+            if git.IsErrNotExist(err) {
+                ctx.NotFound(fmt.Errorf("path '%s' not found in branch '%s'", decodedPath, targetBranch))
+            } else {
+                ctx.ServerError("CheckDirectory", err)
+            }
+            return
+        }
+    }
+    
+    // Set download headers
+    folderName := path.Base(decodedPath)
+    if folderName == "" || folderName == "." || folderName == "/" {
+        folderName = ctx.Repo.Repository.Name
+    }
+    
+    // Determine file extension based on format
+    var fileExt string
+    switch format {
+    case "tar":
+        fileExt = "tar"
+    case "tar.gz":
+        fileExt = "tar.gz"
+    default: // zip
+        fileExt = "zip"
+    }
+    
+    archiveName := fmt.Sprintf("%s-%s.%s", folderName, commit.ID.String()[:7], fileExt)
+    
+    // Set Content-Type based on format
+    switch format {
+    case "tar":
+        ctx.Resp.Header().Set("Content-Type", "application/x-tar")
+    case "tar.gz":
+        ctx.Resp.Header().Set("Content-Type", "application/gzip")
+    default: // zip
+        ctx.Resp.Header().Set("Content-Type", "application/zip")
+    }
+    
+    ctx.Resp.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, archiveName))
+    
+    // Create archive based on format
+    switch format {
+    case "tar":
+        err = createTarArchive(ctx.Resp, commit, decodedPath, false)
+    case "tar.gz":
+        err = createTarArchive(ctx.Resp, commit, decodedPath, true)
+    default: // zip
+        err = createZipArchive(ctx.Resp, commit, decodedPath)
+    }
+    
+    if err != nil {
+        ctx.ServerError("CreateArchive", err)
+        return
+    }
+}
+
+// createZipArchive creates ZIP archive
+func createZipArchive(w io.Writer, commit *git.Commit, treePath string) error {
+    zipWriter := zip.NewWriter(w)
+    defer zipWriter.Close()
+
+    return addFolderToZip(zipWriter, commit, treePath, "")
+}
+
+// createTarArchive creates TAR archive with optional gzip compression
+func createTarArchive(w io.Writer, commit *git.Commit, treePath string, useGzip bool) error {
+    var out io.Writer = w
+    
+    // Apply gzip compression if needed
+    if useGzip {
+        gzipWriter := gzip.NewWriter(w)
+        defer gzipWriter.Close()
+        out = gzipWriter
+    }
+    
+    tarWriter := tar.NewWriter(out)
+    defer tarWriter.Close()
+    
+    return addFolderToTar(tarWriter, commit, treePath, "")
+}
+
+// addFolderToZip recursively adds folder contents to ZIP archive
+func addFolderToZip(zipWriter *zip.Writer, commit *git.Commit, treePath string, zipPath string) error {
+    var entries []*git.TreeEntry
+    var err error
+    
+    if treePath == "" {
+        entries, err = commit.Tree.ListEntries()
+    } else {
+        tree, err := commit.SubTree(treePath)
+        if err != nil {
+            return err
+        }
+        entries, err = tree.ListEntries()
+    }
+    
+    if err != nil {
+        return err
+    }
+
+    for _, entry := range entries {
+        var fullPath string
+        if treePath == "" {
+            fullPath = entry.Name()
+        } else {
+            fullPath = path.Join(treePath, entry.Name())
+        }
+        
+        zipEntryPath := path.Join(zipPath, entry.Name())
+        
+        if entry.IsDir() {
+            // Recursively process subdirectories
+            err = addFolderToZip(zipWriter, commit, fullPath, zipEntryPath)
+            if err != nil {
+                return err
+            }
+        } else {
+            // Add file to archive
+            blob := entry.Blob()
+            dataReader, err := blob.DataAsync()
+            if err != nil {
+                return err
+            }
+            defer dataReader.Close()
+            
+            zipEntry, err := zipWriter.Create(zipEntryPath)
+            if err != nil {
+                return err
+            }
+            
+            _, err = io.Copy(zipEntry, dataReader)
+            if err != nil {
+                return err
+            }
+        }
+    }
+    return nil
+}
+
+// addFolderToTar recursively adds folder contents to TAR archive
+func addFolderToTar(tarWriter *tar.Writer, commit *git.Commit, treePath string, tarPath string) error {
+    var entries []*git.TreeEntry
+    var err error
+    
+    if treePath == "" {
+        entries, err = commit.Tree.ListEntries()
+    } else {
+        tree, err := commit.SubTree(treePath)
+        if err != nil {
+            return err
+        }
+        entries, err = tree.ListEntries()
+    }
+    
+    if err != nil {
+        return err
+    }
+
+    for _, entry := range entries {
+        var fullPath string
+        if treePath == "" {
+            fullPath = entry.Name()
+        } else {
+            fullPath = path.Join(treePath, entry.Name())
+        }
+        
+        tarEntryPath := path.Join(tarPath, entry.Name())
+        
+        if entry.IsDir() {
+            // Create directory entry
+            header := &tar.Header{
+                Name:     tarEntryPath + "/",
+                Mode:     0755,
+                Typeflag: tar.TypeDir,
+            }
+            if err := tarWriter.WriteHeader(header); err != nil {
+                return err
+            }
+            
+            // Recursively process subdirectories
+            err = addFolderToTar(tarWriter, commit, fullPath, tarEntryPath)
+            if err != nil {
+                return err
+            }
+        } else {
+            // Add file to archive
+            blob := entry.Blob()
+            dataReader, err := blob.DataAsync()
+            if err != nil {
+                return err
+            }
+            defer dataReader.Close()
+            
+            // Get file size
+            size := blob.Size()
+            
+            // Create file header
+            header := &tar.Header{
+                Name: tarEntryPath,
+                Mode: 0644,
+                Size: size,
+            }
+            
+            if err := tarWriter.WriteHeader(header); err != nil {
+                return err
+            }
+            
+            // Copy file contents
+            _, err = io.Copy(tarWriter, dataReader)
+            if err != nil {
+                return err
+            }
+        }
+    }
+    return nil
+}
+
 // getBlobForEntry returns blob and last modified time for tree entry
 func getBlobForEntry(ctx *context.Context) (*git.Blob, *time.Time) {