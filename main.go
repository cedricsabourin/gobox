package main

import (
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Storage directory which points to the SSD drive mounted at /mnt/gobox_storage
const StorageDir = "/mnt/gobox_storage"

// Entry is a single file or folder shown in the listing.
type Entry struct {
	Name	string
	// path relative to StorageDir, raw and unescaped
	RelPath string
	// escapePath(RelPath) - safe to drop straight into a link
	URLPath string
	SizeKB  int64
}

// Crumb is one clickable segment of the breadcrumb trail.
type Crumb struct {
	Name	string
	URLPath string
}

// PageData is everything the index template needs to render one folder view.
type PageData struct {
	// current folder relative to StorageDir ("" = root)
	Path	string
	// escaped current path, used by the upload/mkdir actions
	URLPath string
	Crumbs  []Crumb
	Folders []Entry
	Files   []Entry
}

func main() {
	// ensure the storage directory exists on boot
	if err := os.MkdirAll(StorageDir, os.ModePerm); err != nil {
		log.Fatalf("failed to create storage directory: %v", err)
	}

	// direct routes for the webapp
	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/upload", handleUpload)
	http.HandleFunc("/files/", handleFile)
	http.HandleFunc("/rename/", handleRename)
	http.HandleFunc("/mkdir", handleMkdir)
	http.HandleFunc("/folder/", handleFolderDelete)
	http.HandleFunc("/move/", handleMove)

	fmt.Println("GoBox server up on http://localhost:8080")

	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatal(err)
	}
}

// safePath resolves a user-supplied relative path inside StorageDir. prefixing a
// "/" before cleaning collapses any ".." so the result can never climb above the
// root - that's what keeps a request from wandering outside the storage dir. it
// returns the absolute path, the canonical relative path ("" for root), and
// whether the path was safe.
func safePath(rel string) (string, string, bool) {
	clean := filepath.Clean("/" + rel)
	full := filepath.Join(StorageDir, clean)

	if full != StorageDir && !strings.HasPrefix(full, StorageDir+string(os.PathSeparator)) {
		return "", "", false
	}

	return full, strings.TrimPrefix(clean, "/"), true
}

// escapePath url-escapes each segment of a relative path but keeps the slashes,
// so a nested path can sit in a url like /files/photos/2024/clip.mp4.
func escapePath(rel string) string {
	if rel == "" {
		return ""
	}

	parts := strings.Split(rel, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// buildCrumbs turns a relative folder path into breadcrumb links, starting with a
// "Home" crumb that points back at the storage root.
func buildCrumbs(clean string) []Crumb {
	crumbs := []Crumb{{Name: "Home", URLPath: ""}}

	if clean == "" {
		return crumbs
	}

	var acc string
	for _, seg := range strings.Split(clean, "/") {
		acc = path.Join(acc, seg)
		crumbs = append(crumbs, Crumb{
			Name:    seg,
			URLPath: escapePath(acc),
		})
	}
	return crumbs
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	// 1. resolve the requested folder safely inside the storage dir
	full, clean, ok := safePath(r.URL.Query().Get("path"))

	if !ok {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}

	// 2. make sure it's actually a directory before listing it
	info, err := os.Stat(full)

	if err != nil || !info.IsDir() {
		http.NotFound(w, r)
		return
	}

	entries, err := os.ReadDir(full)

	if err != nil {
		http.Error(w, "Filesystem read failure: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// 3. split entries into folders and files, recording each one's rel path
	var folders, files []Entry
	for _, entry := range entries {
		rel := path.Join(clean, entry.Name())

		if entry.IsDir() {
			folders = append(folders, Entry{
				Name:    entry.Name(),
				RelPath: rel,
				URLPath: escapePath(rel),
			})
			continue
		}

		meta, err := entry.Info()

		if err != nil {
			continue // skip files with unresolved metadata read issues
		}

		files = append(files, Entry{
			Name:    entry.Name(),
			RelPath: rel,
			URLPath: escapePath(rel),
			SizeKB:  meta.Size() / 1024,
		})
	}

	// 4. bundle everything (incl. the breadcrumb trail) for the template
	data := PageData{
		Path:    clean,
		URLPath: escapePath(clean),
		Crumbs:  buildCrumbs(clean),
		Folders: folders,
		Files:   files,
	}

	tmpl, err := template.ParseFiles("templates/index.html")

	if err != nil {
		http.Error(w, "Template compilation error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// 5. inject the dataset directly into the hypermedia component
	tmpl.Execute(w, data)
}

// handleFile serves a single stored file on GET (inline preview, or a forced
// save with ?download=1) and removes it on DELETE. the file may live in a nested
// folder, so the path after /files/ is resolved as a relative path.
func handleFile(w http.ResponseWriter, r *http.Request) {
	// 1. resolve the (possibly nested) path safely inside the storage dir
	full, clean, ok := safePath(strings.TrimPrefix(r.URL.Path, "/files/"))

	if !ok || clean == "" {
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodDelete:
		// 2. remove the file from disk. 404 if it was already gone, 500 otherwise.
		if err := os.Remove(full); err != nil {
			if os.IsNotExist(err) {
				http.NotFound(w, r)
			} else {
				http.Error(w, "Failed to delete file", http.StatusInternalServerError)
			}
			return
		}

		// 3. empty 200 tells htmx to swap the list row out.
		w.WriteHeader(http.StatusOK)

	case http.MethodGet:
		// 2. if the download flag is set, tell the browser to save rather than render
		if r.URL.Query().Get("download") == "1" {
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(clean)))
		}

		// 3. ServeFile streams the bytes, sniffs the content type, 404s if missing,
		// and honors HTTP range requests so videos can be scrubbed/seeked without
		// downloading the whole file first.
		http.ServeFile(w, r, full)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleRename renames a file or folder in place. the new name arrives in the
// HX-Prompt header (set by htmx's hx-prompt); on success we return the matching
// row - file or folder - so htmx can swap it into the list.
func handleRename(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 1. resolve the existing item's path safely
	full, clean, ok := safePath(strings.TrimPrefix(r.URL.Path, "/rename/"))

	if !ok || clean == "" {
		http.NotFound(w, r)
		return
	}

	// 2. desired name from the prompt header. Base() keeps it a pure name so a
	// rename can't sneak the item into another folder or climb out.
	newName := filepath.Base(strings.TrimSpace(r.Header.Get("HX-Prompt")))

	if newName == "" || newName == "." {
		http.Error(w, "A new name is required", http.StatusBadRequest)
		return
	}

	// 3. the renamed item stays in its current parent folder
	newFull := filepath.Join(filepath.Dir(full), newName)

	// 4. refuse to clobber an existing entry
	if _, err := os.Stat(newFull); err == nil {
		http.Error(w, "Something with that name already exists", http.StatusConflict)
		return
	}

	// 5. do the rename. 404 if the original is gone, 500 otherwise.
	if err := os.Rename(full, newFull); err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
		} else {
			http.Error(w, "Failed to rename", http.StatusInternalServerError)
		}
		return
	}

	// 6. hand back the right row markup so htmx swaps it in place. the new rel
	// path is the old parent's rel path joined with the new name.
	newRel := path.Join(path.Dir(clean), newName)
	w.Header().Set("Content-Type", "text/html")

	info, err := os.Stat(newFull)

	if err == nil && info.IsDir() {
		fmt.Fprint(w, folderRowHTML(newRel, newName))
		return
	}

	var sizeKB int64
	if err == nil {
		sizeKB = info.Size() / 1024
	}
	fmt.Fprint(w, fileRowHTML(newRel, newName, sizeKB))
}

// handleMkdir creates a new folder inside the current one. the folder name comes
// from the HX-Prompt header; afterward we ask htmx to refresh the page so the new
// folder shows up in its sorted spot in the listing.
func handleMkdir(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 1. resolve the current folder, where the new one will be created
	dir, _, ok := safePath(r.URL.Query().Get("path"))

	if !ok {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}

	// 2. folder name from the prompt. Base() keeps it a single path segment.
	name := filepath.Base(strings.TrimSpace(r.Header.Get("HX-Prompt")))

	if name == "" || name == "." {
		http.Error(w, "A folder name is required", http.StatusBadRequest)
		return
	}

	// 3. create it, refusing if something with that name already exists
	target := filepath.Join(dir, name)

	if _, err := os.Stat(target); err == nil {
		http.Error(w, "Something with that name already exists", http.StatusConflict)
		return
	}

	if err := os.Mkdir(target, os.ModePerm); err != nil {
		http.Error(w, "Failed to create folder", http.StatusInternalServerError)
		return
	}

	// 4. HX-Refresh reloads the whole page so the new folder lands in sorted order
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusOK)
}

// handleFolderDelete removes a folder and everything inside it. it's kept separate
// from handleFile on purpose - os.RemoveAll wipes a whole subtree, so it deserves
// its own clearly-named handler rather than hiding inside the file route.
func handleFolderDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 1. resolve the folder path safely
	full, clean, ok := safePath(strings.TrimPrefix(r.URL.Path, "/folder/"))

	if !ok || clean == "" {
		http.NotFound(w, r)
		return
	}

	// 2. remove the whole subtree
	if err := os.RemoveAll(full); err != nil {
		http.Error(w, "Failed to delete folder", http.StatusInternalServerError)
		return
	}

	// 3. empty 200 so htmx swaps the folder row out of the list
	w.WriteHeader(http.StatusOK)
}

// handleMove moves a file into another folder. the destination folder path comes
// from the HX-Prompt header. once the file leaves the current view we return an
// empty 200 so htmx drops the row; if the destination is the file's current
// folder we hand the unchanged row back instead.
func handleMove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 1. resolve the source file
	srcFull, srcClean, ok := safePath(strings.TrimPrefix(r.URL.Path, "/move/"))

	if !ok || srcClean == "" {
		http.NotFound(w, r)
		return
	}

	// 2. resolve the destination folder from the prompt; it must already exist
	destDir, _, ok := safePath(strings.TrimSpace(r.Header.Get("HX-Prompt")))

	if !ok {
		http.Error(w, "Invalid destination", http.StatusBadRequest)
		return
	}

	info, err := os.Stat(destDir)

	if err != nil || !info.IsDir() {
		http.Error(w, "Destination folder does not exist", http.StatusNotFound)
		return
	}

	name := filepath.Base(srcFull)

	// 3. if the destination is the file's current folder nothing moves, so just
	// return the unchanged row and htmx leaves it in place.
	if filepath.Dir(srcFull) == destDir {
		var sizeKB int64
		if fi, err := os.Stat(srcFull); err == nil {
			sizeKB = fi.Size() / 1024
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, fileRowHTML(srcClean, name, sizeKB))
		return
	}

	// 4. refuse to clobber a file already sitting in the destination
	destFull := filepath.Join(destDir, name)

	if _, err := os.Stat(destFull); err == nil {
		http.Error(w, "A file with that name already exists in the destination", http.StatusConflict)
		return
	}

	// 5. do the move. 404 if the source vanished, 500 otherwise.
	if err := os.Rename(srcFull, destFull); err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
		} else {
			http.Error(w, "Failed to move file", http.StatusInternalServerError)
		}
		return
	}

	// 6. the file left the current folder, so an empty 200 drops the row
	w.WriteHeader(http.StatusOK)
}

// fileRowHTML renders the list-item markup for a single stored file. it's shared
// by the upload, rename, and move responses so the row only has to be maintained
// in one place (the {{range .Files}} block in index.html mirrors it for the
// initial page load).
func fileRowHTML(relPath, name string, sizeKB int64) string {
	// escape the path for the links; the raw name is fine as display text
	esc := escapePath(relPath)
	return fmt.Sprintf(`
		    <li class="entry-row flex items-center justify-between gap-3 border-b border-line px-4 py-2.5 hover:bg-stone-50">
		        <a href="/files/%s" target="_blank" class="truncate font-medium text-stone-800 hover:text-stone-950">%s</a>
		        <div class="ml-4 flex shrink-0 items-center gap-3">
		            <span class="text-xs text-stone-500">%d KB</span>
		            <a href="/files/%s?download=1" class="text-xs font-medium text-green-600 hover:text-green-700">Download</a>
		            <button hx-post="/move/%s"
		                    hx-prompt="Move to folder (e.g. photos/2024):"
		                    hx-target="closest li"
		                    hx-swap="outerHTML"
		                    class="text-xs font-medium text-stone-500 hover:text-sky-600">Move</button>
		            <button hx-post="/rename/%s"
		                    hx-prompt="New name for %s:"
		                    hx-target="closest li"
		                    hx-swap="outerHTML"
		                    class="text-xs font-medium text-stone-500 hover:text-stone-900">Rename</button>
		            <button hx-delete="/files/%s"
		                    hx-target="closest li"
		                    hx-swap="outerHTML"
		                    hx-confirm="Delete %s?"
		                    class="text-xs font-medium text-stone-500 hover:text-red-600">Delete</button>
		        </div>
		    </li>`, esc, name, sizeKB, esc, esc, esc, name, esc, name)
}

// folderRowHTML renders the list-item markup for a folder: the name navigates
// into it, while rename/delete act on the whole folder. the {{range .Folders}}
// block in index.html mirrors this for the initial page load.
func folderRowHTML(relPath, name string) string {
	esc := escapePath(relPath)
	return fmt.Sprintf(`
		    <li class="entry-row flex items-center justify-between gap-3 border-b border-line px-4 py-2.5 hover:bg-stone-50">
		        <a href="/?path=%s" class="flex min-w-0 items-center gap-2 font-medium text-stone-800 hover:text-stone-950">
		            <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="currentColor" class="h-4 w-4 shrink-0 text-stone-500"><path d="M3.75 6A2.25 2.25 0 0 1 6 3.75h3.379a1.5 1.5 0 0 1 1.06.44l1.122 1.121a1.5 1.5 0 0 0 1.06.439H18A2.25 2.25 0 0 1 20.25 7.5v9A2.25 2.25 0 0 1 18 18.75H6A2.25 2.25 0 0 1 3.75 16.5V6Z" /></svg>
		            <span class="truncate">%s</span>
		        </a>
		        <div class="ml-4 flex shrink-0 items-center gap-3">
		            <button hx-post="/rename/%s"
		                    hx-prompt="New name for %s:"
		                    hx-target="closest li"
		                    hx-swap="outerHTML"
		                    class="text-xs font-medium text-stone-500 hover:text-stone-900">Rename</button>
		            <button hx-delete="/folder/%s"
		                    hx-target="closest li"
		                    hx-swap="outerHTML"
		                    hx-confirm="Delete folder %s and everything inside it?"
		                    class="text-xs font-medium text-stone-500 hover:text-red-600">Delete</button>
		        </div>
		    </li>`, esc, name, esc, name, esc, name)
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 1. resolve the destination folder from the query so uploads land in the
	// folder the user is currently viewing
	dir, clean, ok := safePath(r.URL.Query().Get("path"))

	if !ok {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}

	// 2. get the multipart reader for raw socket streaming
	reader, err := r.MultipartReader()

	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	for {
		part, err := reader.NextPart()

		if err == io.EOF {
			break // entire request stream parsed successfully
		}

		if err != nil {
			http.Error(w, "Error parsing file stream", http.StatusInternalServerError)
			return
		}

		defer part.Close()

		// skip structural form fields; focus on the file payload
		if part.FileName() == "" {
			continue
		}

		// 3. create the destination file inside the current folder. Base() keeps
		// a browser-supplied name from escaping into another directory.
		name := filepath.Base(part.FileName())
		dst, err := os.Create(filepath.Join(dir, name))

		if err != nil {
			http.Error(w, "Failed to create local destination file", http.StatusInternalServerError)
			return
		}
		defer dst.Close()

		// 4. stream network bytes directly to storage.
		// io.Copy works via small 32KB chunk cycles, meaning memory usage
		// stays near zero even if we upload a massive 4GB video file.
		written, err := io.Copy(dst, part)

		if err != nil {
			http.Error(w, "Upload interrupted mid-stream", http.StatusInternalServerError)
			return
		}

		log.Printf("Successfully archived: %s (%d bytes)", name, written)

		// 5. return the row for the new file (the shared helper keeps it identical
		// to the rename/move responses), using its folder-aware relative path.
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, fileRowHTML(path.Join(clean, name), name, written/1024))
	}
}
