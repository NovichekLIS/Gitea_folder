--- a/routers/web/repo/download.go
+++ b/routers/web/repo/download.go
@@ -XXX,XXX +XXX,XXX @@
+func DownloadFolder(ctx *context.Context) {
+
+	treePath := ctx.PathParam("*")
+	
+	branchName := ctx.PathParam("branchname")
+	
+	format := ctx.Req.URL.Query().Get("format")
+	if format == "" {
+		format = "zip" // default
+	}
+	
+	if treePath == "" && ctx.Repo.TreePath != "" {
+		treePath = ctx.Repo.TreePath
+	}
+	
+	decodedPath, err := url.PathUnescape(treePath)
+	if err != nil {
+		decodedPath = treePath
+	}
+
+	decodedPath = strings.TrimPrefix(decodedPath, "/")
+	
+	if decodedPath == "." {
+		decodedPath = ""
+	}
+
+	if ctx.Repo.Repository == nil || ctx.Repo.GitRepo == nil {
+		ctx.NotFound(fmt.Errorf("repository not found"))
+		return
+	}
+
+	var targetBranch string
+	if branchName != "" {
+		targetBranch = branchName
+	} else if ctx.Repo.Commit != nil {
+		targetBranch = ctx.Repo.Repository.DefaultBranch
+		if targetBranch == "" {
+			targetBranch = "main"
+		}
+	} else {
+		targetBranch = ctx.Repo.Repository.DefaultBranch
+		if targetBranch == "" {
+			targetBranch = "main"
+		}
+	}
+	
+	commit, err := ctx.Repo.GitRepo.GetCommit(targetBranch)
+	if err != nil {
+		ctx.ServerError("GetCommit", err)
+		return
+	}	
+	if decodedPath != "" {
+		_, err := commit.SubTree(decodedPath)
+		if err != nil {
+			if git.IsErrNotExist(err) {
+				ctx.NotFound(fmt.Errorf("path '%s' not found in branch '%s'", decodedPath, targetBranch))
+			} else {
+				ctx.ServerError("CheckDirectory", err)
+			}
+			return
+		}
+	}
+	
+	folderName := path.Base(decodedPath)
+	if folderName == "" || folderName == "." || folderName == "/" {
+		folderName = ctx.Repo.Repository.Name
+	}
+	
+	var fileExt string
+	switch strings.ToLower(format) {
+	case "tar":
+		fileExt = "tar"
+	case "tar.gz", "tgz", "gz":
+		fileExt = "tar.gz"
+	default: 
+		fileExt = "zip"
+	}
+	
+	archiveName := fmt.Sprintf("%s-%s.%s", folderName, commit.ID.String()[:7], fileExt)
+	
+	switch strings.ToLower(format) {
+	case "tar":
+		ctx.Resp.Header().Set("Content-Type", "application/x-tar")
+	case "tar.gz", "tgz", "gz":
+		ctx.Resp.Header().Set("Content-Type", "application/gzip")
+	default: // zip
+		ctx.Resp.Header().Set("Content-Type", "application/zip")
+	}
+	
+	ctx.Resp.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, archiveName))
+	
+	err = createGitArchiveBuffered(ctx.Resp, ctx.Repo.GitRepo.Path, commit.ID.String(), decodedPath, format)
+	if err != nil {
+		ctx.ServerError("CreateArchive", err)
+		return
+	}
+}
+
+func createGitArchiveBuffered(w io.Writer, repoPath string, commitHash string, treePath string, format string) error {
+	if _, err := exec.LookPath("git"); err != nil {
+		return fmt.Errorf("git not found: %v", err)
+	}
+	
+	var formatArg string
+	switch strings.ToLower(format) {
+	case "tar":
+		formatArg = "tar"
+	case "tar.gz", "tgz", "gz":
+		formatArg = "tar.gz"
+	default:
+		formatArg = "zip"
+	}
+	
+	args := []string{"archive", "--format=" + formatArg, commitHash}
+	if treePath != "" && treePath != "." {
+		args = append(args, strings.TrimPrefix(treePath, "/"))
+	}
+	
+	cmd := exec.Command("git", args...)
+	cmd.Dir = repoPath
+	
+	stdout, err := cmd.StdoutPipe()
+	if err != nil {
+		return fmt.Errorf("failed to create stdout pipe: %v", err)
+	}
+	
+	var stderr bytes.Buffer
+	cmd.Stderr = &stderr
+	
+	if err := cmd.Start(); err != nil {
+		return fmt.Errorf("failed to start git: %v", err)
+	}
+	_, copyErr := io.Copy(w, stdout)
+	
+	waitErr := cmd.Wait()
+	
+	if copyErr != nil {
+		return fmt.Errorf("failed to copy archive data: %v", copyErr)
+	}
+	
+	if waitErr != nil {
+		return fmt.Errorf("git archive failed: %v\nstderr: %s", waitErr, stderr.String())
+	}
+	
+	return nil
}