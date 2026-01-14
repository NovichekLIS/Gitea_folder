--- a/routers/web/web.go
+++ b/routers/web/web.go
@@ -XXX,XXX +XXX,XXX @@
 m.Group("/{username}/{reponame}", func() {
     // ... existing routes ...
     
+    // Folder download routes with branch support
+    m.Get("/download/folder/branch/{branchname}/*", repo.MustBeNotEmpty, repo.DownloadFolder)
     m.Get("/download/folder/*", repo.MustBeNotEmpty, repo.DownloadFolder)
     
     // ... other routes ...
 }, optSignIn, context.RepoAssignment, reqUnitCodeReader)