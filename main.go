package main

import (
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Storage directory which points to the SSD drive mounted at /mnt/gobox_storage
const StorageDir = "/mnt/gobox_storage"

//FileAsset holds metadata for server-side template rendering
type FileAsset struct {
	Name	string
	// url-safe version of the name for use in links (handles spaces, etc.)
	URLName string
	SizeKB  int64
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

	fmt.Println("GoBox server up on http://localhost:8080")

	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatal(err)
	}
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	// 1. read block storage directory contents
	entries, err := os.ReadDir(StorageDir)

	if err != nil {
		http.Error(w, "Filesystem read failure: " + err.Error(), http.StatusInternalServerError)
		return
	}

	// 2. parse file metadata into an array of structs
	var assets []FileAsset
	for _, entry := range entries {
		if entry.IsDir() {
			continue // skip subdirectories
		}

		info, err := entry.Info()

		if err != nil {
			continue // skip files with unresolves metadata read issues
		}

		assets = append(assets, FileAsset {
			Name: entry.Name(),
			// escape the name so links work even with spaces or odd characters
			URLName: url.PathEscape(entry.Name()),
			SizeKB: info.Size() / 1024,
		})
	}

	tmpl, err := template.ParseFiles("templates/index.html")

	if err != nil {
		http.Error(w, "Template compilation error: " + err.Error(), http.StatusInternalServerError)
		return
	}

	// 3, inject the dataset directly into the hypermedia component
	tmpl.Execute(w, assets)
}

// handleFile serves a single stored file on GET (inline preview, or a forced
// save with ?download=1) and removes it on DELETE.
func handleFile(w http.ResponseWriter, r *http.Request) {
	// 1. pull the filename out of the path. filepath.Base strips any directory
	// parts so a request can't wander outside the storage dir.
	name := filepath.Base(strings.TrimPrefix(r.URL.Path, "/files/"))

	if name == "" || name == "." {
		http.NotFound(w, r)
		return
	}

	path := filepath.Join(StorageDir, name)

	switch r.Method {
	case http.MethodDelete:
		// 2. remove the file from disk. report 404 if it was already gone,
		// otherwise treat it as a server-side failure.
		if err := os.Remove(path); err != nil {
			if os.IsNotExist(err) {
				http.NotFound(w, r)
			} else {
				http.Error(w, "Failed to delete file", http.StatusInternalServerError)
			}
			return
		}

		// 3. respond with an empty 200. the empty body is what tells htmx to
		// swap the list row out (replacing it with nothing).
		w.WriteHeader(http.StatusOK)

	case http.MethodGet:
		// 2. if the download flag is set, tell the browser to save rather than render
		if r.URL.Query().Get("download") == "1" {
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
		}

		// 3. ServeFile does the heavy lifting: it streams the bytes, sniffs the
		// content type, returns a 404 if missing, and crucially honors HTTP range
		// requests so videos can be scrubbed/seeked without downloading the whole file.
		http.ServeFile(w, r, path)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleRename renames a stored file in place. the new name arrives in the
// HX-Prompt header (set by htmx's hx-prompt) and on success we return the
// updated row so htmx can swap it into the list.
func handleRename(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 1. current filename from the path. filepath.Base strips any directory
	// parts so the request can't point outside the storage dir.
	old := filepath.Base(strings.TrimPrefix(r.URL.Path, "/rename/"))

	if old == "" || old == "." {
		http.NotFound(w, r)
		return
	}

	// 2. desired filename from the htmx prompt header. Base it too so a typed
	// path like "../foo" collapses to just "foo" and stays an in-place rename.
	newName := filepath.Base(strings.TrimSpace(r.Header.Get("HX-Prompt")))

	if newName == "" || newName == "." {
		http.Error(w, "A new name is required", http.StatusBadRequest)
		return
	}

	oldPath := filepath.Join(StorageDir, old)
	newPath := filepath.Join(StorageDir, newName)

	// 3. refuse to clobber an existing file. os.Stat succeeding means the target
	// name is already taken, so bail out instead of silently overwriting it.
	if _, err := os.Stat(newPath); err == nil {
		http.Error(w, "A file with that name already exists", http.StatusConflict)
		return
	}

	// 4. do the rename. report 404 if the original is gone, 500 otherwise.
	if err := os.Rename(oldPath, newPath); err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
		} else {
			http.Error(w, "Failed to rename file", http.StatusInternalServerError)
		}
		return
	}

	// 5. look up the size for the refreshed row, then hand back the row markup
	// so htmx swaps the renamed entry in place.
	var sizeKB int64
	if info, err := os.Stat(newPath); err == nil {
		sizeKB = info.Size() / 1024
	}

	w.Header().Set("Content-Type", "text/html")
	fmt.Fprint(w, fileRowHTML(newName, sizeKB))
}

// fileRowHTML renders the list-item markup for a single stored file. it's shared
// by the upload and rename responses so the row only has to be maintained in one
// place (the {{range}} block in index.html mirrors this for the initial page load).
func fileRowHTML(name string, sizeKB int64) string {
	// escape the name for the links; the raw name is fine as display text
	urlName := url.PathEscape(name)
	return fmt.Sprintf(`
		    <li class="flex items-center justify-between rounded-xl border border-line bg-panel px-4 py-3">
		        <a href="/files/%s" target="_blank" class="truncate font-medium text-stone-200 hover:text-amber-400">%s</a>
		        <div class="ml-4 flex shrink-0 items-center gap-3">
		            <span class="text-xs text-stone-500">%d KB</span>
		            <a href="/files/%s?download=1" class="text-xs font-medium text-green-400 hover:text-lime-300">Download</a>
		            <button hx-post="/rename/%s"
		                    hx-prompt="New name for %s:"
		                    hx-target="closest li"
		                    hx-swap="outerHTML"
		                    class="text-xs font-medium text-stone-500 hover:text-amber-400">Rename</button>
		            <button hx-delete="/files/%s"
		                    hx-target="closest li"
		                    hx-swap="outerHTML"
		                    hx-confirm="Delete %s?"
		                    class="text-xs font-medium text-stone-500 hover:text-red-400">Delete</button>
		        </div>
		    </li>`, urlName, name, sizeKB, urlName, urlName, name, urlName, name)
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 1. get the multipart reader for raw socket streaming
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

	
		// 2. create the destination file on the disk
		dstPath := filepath.Join(StorageDir, part.FileName())
		dst, err := os.Create(dstPath)

		if err != nil {
			http.Error(w, "Failed to create local destination file", http.StatusInternalServerError)
			return
		}
		defer dst.Close()

	
		// 3. stream network bytes directly to storage.
		// io.Copy works via small 32KB chunk cycles, meaning memory usage
		// stays near zero even if we upload a massive 4GB video file.
		written, err := io.Copy(dst, part)

		if err != nil {
			http.Error(w, "Upload interrupted mid-stream", http.StatusInternalServerError)
			return
		}
	
		log.Printf("Successfully archived: %s (%d bytes)", part.FileName(), written)
			
		// 4. Return an HTMX fragment to dynamically update the file list.
		// the shared helper keeps this row identical to the rename response.
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, fileRowHTML(part.FileName(), written/1024))
	}
}
