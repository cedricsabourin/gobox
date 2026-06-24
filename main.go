package main

import (
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
)

// Temporary mock storage directory on the SD card
const StorageDir = "./data"

//FileAsset holds metadata for server-side template rendering
type FileAsset struct {
	Name	string
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
			
		// 4. Return an HTMX fragment to dynamically update the UI list
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `
		    <div class="flex items-center justify-between p-3 bg-zinc-900 ...">
		        <span class="truncate text-zinc-300 font-medium">%s</span>
		        <span class="text-zinc-500 text-[11px] shrink-0 ml-4">%d KB</span>
		    </div>`, part.FileName(), written/1024)
	}
}
