package main

import (
	"context"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"sync"

	"github.com/abema/go-mp4"
	"github.com/eduncan911/podcast"
	"github.com/gomarkdown/markdown"
	"github.com/gomarkdown/markdown/html"
	"github.com/gomarkdown/markdown/parser"
	"github.com/spf13/viper"
	"github.com/tcolgate/mp3"
	"tailscale.com/tsnet"
)

// Config holds all application configuration settings.
type Config struct {
	Port         string `mapstructure:"port"`
	BaseURL      string `mapstructure:"base_url"`
	AudioFolder  string `mapstructure:"audio_folder"`
	FeedFileName string
	SortMethod   string
	Tailscale    bool `mapstructure:"tailscale"`
	Verbose      bool `mapstructure:"verbose"`
	Podcast      struct {
		Title       string `mapstructure:"title"`
		Description string `mapstructure:"description"`
		Link        string
		Author      string `mapstructure:"author"`
		Email       string `mapstructure:"email"`
		Language    string `mapstructure:"language"`
		Category    string `mapstructure:"category"`
		SubCategory string `mapstructure:"sub_category"`
		Explicit    string `mapstructure:"explicit"`
		Copyright   string `mapstructure:"copyright"`
		Type        string `mapstructure:"type"` // episodic or serial
		OwnerName   string `mapstructure:"owner_name"`
		OwnerEmail  string `mapstructure:"owner_email"`
		Image       string `mapstructure:"image"` // Optional remote URL override
	} `mapstructure:"podcast"`
}

var (
	cfg         Config
	feedManager *FeedManager
)

// FeedManager handles the podcast feed and ensures thread-safe access.
type FeedManager struct {
	feed *podcast.Podcast
	mu   sync.RWMutex
}

func (fm *FeedManager) Update(p *podcast.Podcast) {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	fm.feed = p
}

func (fm *FeedManager) Get() *podcast.Podcast {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	return fm.feed
}

func main() {
	useTailscale := flag.Bool("tailscale", false, "Enable Tailscale Funnel support")
	verbose := flag.Bool("verbose", false, "Enable verbose logging for Tailscale")
	configPath := flag.String("config", "", "Path to the configuration file (optional)")
	audioPath := flag.String("audio", "", "Path to the audio files directory (overrides config)")
	portFlag := flag.String("port", "", "Port to listen on (overrides config)")
	baseUrlFlag := flag.String("baseurl", "", "Base URL for the podcast (overrides config)")
	sortMethod := flag.String("sort", "date", "Sort method: filename, date, or custom")
	initConfig := flag.Bool("init", false, "Generate a default configuration file and exit")
	flag.Parse()

	if *initConfig {
		generateDefaultConfig(*configPath)
		return
	}

	loadConfig(*configPath)
	ensureTemplateExists()

	// Override audio folder if flag is provided
	if *audioPath != "" {
		cfg.AudioFolder = *audioPath
	}
	
	// Override port if flag is provided
	if *portFlag != "" {
		cfg.Port = *portFlag
		// If base URL wasn't explicitly set, update it to use the new port
		if *baseUrlFlag == "" {
			cfg.BaseURL = fmt.Sprintf("http://localhost:%s", *portFlag)
		}
	}
	
	// Override base URL if flag is provided (takes precedence over port-based URL)
	if *baseUrlFlag != "" {
		cfg.BaseURL = *baseUrlFlag
	}
	
	// Override sort method if flag is provided (or use default)
	cfg.SortMethod = *sortMethod
	
	// Override Tailscale if flag is provided
	if *useTailscale {
		cfg.Tailscale = true
	}
	
	// Override Verbose if flag is provided
	if *verbose {
		cfg.Verbose = true
	}

	// Ensure audio folder defaults to ./audio if empty
	if cfg.AudioFolder == "" {
		cfg.AudioFolder = "./audio"
	}
	
	// Recalculate the feed link after all URL/port overrides
	cfg.Podcast.Link = cfg.BaseURL + "/" + cfg.FeedFileName


	var listener net.Listener

	if cfg.Tailscale {
		hostname := "podcast-server"
		
		// Store state alongside config.yaml in the current directory
		stateDir := filepath.Join(".", ".tsnet-state")
		
		if err := os.MkdirAll(stateDir, 0700); err != nil {
			log.Fatalf("Could not create tsnet state directory: %v", err)
		}

		s := &tsnet.Server{
			Hostname: hostname,
			Dir:      stateDir,
			Logf:     func(format string, args ...any) {}, // Quiet logs by default
		}
		if cfg.Verbose {
			s.Logf = log.Printf
		}
		defer s.Close()

		// Wait for the server to start up and get its identity
		log.Println("Connecting to Tailscale...")
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		st, err := s.Up(ctx)
		if err != nil {
			log.Fatalf("Error waiting for Tailscale to come up: %v", err)
		}

			if st.Self.Online {
				dnsName := strings.TrimSuffix(st.Self.DNSName, ".")
				cfg.BaseURL = fmt.Sprintf("https://%s", dnsName)
				// Re-calculate derived fields
				cfg.Podcast.Link = cfg.BaseURL + "/" + cfg.FeedFileName

				log.Printf("Tailscale node is online: %s", dnsName)
				log.Printf("Capabilities: %v", st.Self.Capabilities)
				
				hasFunnel := false
				for _, cap := range st.Self.Capabilities {
					if strings.Contains(string(cap), "funnel") {
						hasFunnel = true
						break
					}
				}
				
				if hasFunnel {
					log.Printf("Funnel capability detected! Public access should be working.")
				} else {
					log.Printf("WARNING: Funnel capability NOT detected. Check your ACLs and Admin Console.")
				}

				log.Printf("Base URL overridden to: %s", cfg.BaseURL)
			log.Printf("Access your podcast feed at: %s", cfg.Podcast.Link)
			log.Printf("If Funnel is enabled in ACLs, it is available at the same URL publicly.")
		}

		ln, err := s.ListenFunnel("tcp", ":443")
		if err != nil {
			log.Fatal(err)
		}
		listener = ln
	}

	// Initialize FeedManager
	feedManager = &FeedManager{}
	
	// Initial feed creation
	p := createPodcastFeed()
	feedManager.Update(p)

	// Start watching for changes
	go watchAudioFolder()

	// 0. Handler for the HTML landing page (root path)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Only handle exact root path
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		
		serveIndexPage(w, r)
	})

	// 1. Handler for the RSS feed
	http.HandleFunc("/"+cfg.FeedFileName, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		
		currentFeed := feedManager.Get()
		if currentFeed == nil {
			http.Error(w, "Feed not ready", http.StatusServiceUnavailable)
			return
		}
		
		if err := currentFeed.Encode(w); err != nil {
			log.Printf("Error encoding feed: %v", err)
			http.Error(w, "Could not generate feed", http.StatusInternalServerError)
		}
	})

	// 2. Handler for the audio files
	audioHandler := http.StripPrefix("/episodes/", http.FileServer(http.Dir(cfg.AudioFolder)))
	http.Handle("/episodes/", audioHandler)

	// Logging based on configuration
	serverPort := cfg.Port
	if serverPort == "" {
		// Try to parse port from BaseURL
		u, err := url.Parse(cfg.BaseURL)
		if err == nil {
			serverPort = u.Port()
		}
		// If still empty (e.g. http://example.com), default to 8080
		if serverPort == "" {
			serverPort = "8080"
		}
	}
	// Ensure it has a colon
	if !strings.HasPrefix(serverPort, ":") {
		serverPort = ":" + serverPort
	}

	if !cfg.Tailscale {
		log.Printf("Podcast Server running at %s", cfg.BaseURL)
		log.Printf("Listening on port %s", serverPort)
		log.Printf("RSS Feed URL: %s", cfg.Podcast.Link)
	}
	log.Printf("Serving files from local directory: %s", cfg.AudioFolder)

	// Start the server
	if cfg.Tailscale {
		if err := http.Serve(listener, nil); err != nil {
			log.Fatal(err)
		}
	} else {
		if err := http.ListenAndServe(serverPort, nil); err != nil {
			log.Fatal("ListenAndServe: ", err)
		}
	}
}

// setDefaults sets the initial, sensible default values for the application.
func setDefaults(v *viper.Viper) {
	v.SetDefault("base_url", "http://localhost:8080")
	v.SetDefault("audio_folder", "./audio")
	v.SetDefault("tailscale", false)
	v.SetDefault("verbose", false)
	v.SetDefault("podcast.title", "My Podcast")
	v.SetDefault("podcast.description", "A podcast generated automatically from a folder of audio files.")
	v.SetDefault("podcast.author", "Podcast Author")
	v.SetDefault("podcast.email", "podcastauthor@example.com")
	v.SetDefault("podcast.language", "en-us")
	v.SetDefault("podcast.category", "Technology")
	v.SetDefault("podcast.sub_category", "Software Development")
	v.SetDefault("podcast.explicit", "no")
	v.SetDefault("podcast.type", "episodic")
	v.SetDefault("podcast.copyright", "")
	v.SetDefault("podcast.owner_name", "")
	v.SetDefault("podcast.owner_email", "")
	v.SetDefault("podcast.image", "")
}

// loadConfig initializes Viper, reads the configuration file.
// If path is empty, it looks for "config.yaml" in the current directory but doesn't create it if missing.
// If path is provided, it tries to read it.
func loadConfig(path string) {
	v := viper.New()
	setDefaults(v)

	if path != "" {
		v.SetConfigFile(path)
	} else {
		v.SetConfigName("podcast-server")
		v.AddConfigPath(".")
		v.SetConfigType("yaml")
	}

	v.SetEnvPrefix("PODCAST")
	v.AutomaticEnv()

	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			// Config file not found.
			// If user specified a path, warn them.
			// If default path, just log that we are using defaults.
			if path != "" {
				log.Printf("Warning: Configuration file %s not found. Using defaults.", path)
			} else {
				log.Println("No configuration file found. Using defaults and flags.")
			}
		} else {
			// If the error is "file not found" but not the specific viper type (can happen with SetConfigFile), check os.Stat
			if path != "" {
				if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
					log.Printf("Warning: Configuration file %s not found. Using defaults.", path)
				} else {
					log.Fatalf("Fatal error reading config file: %s", err)
				}
			} else {
				// Default path error (other than not found)
				log.Fatalf("Fatal error reading config file: %s", err)
			}
		}
	} else {
		log.Printf("Configuration file loaded from: %s", v.ConfigFileUsed())
	}

	if err := v.Unmarshal(&cfg); err != nil {
		log.Fatalf("Unable to unmarshal config into struct: %s", err)
	}

	cfg.BaseURL = strings.TrimSuffix(cfg.BaseURL, "/")
	cfg.FeedFileName = "feed.xml"
	cfg.Podcast.Link = cfg.BaseURL + "/" + cfg.FeedFileName
}

// generateDefaultConfig writes a default configuration file to the specified path (or config.yaml)
func generateDefaultConfig(path string) {
	v := viper.New()
	setDefaults(v)
	
	targetPath := path
	if targetPath == "" {
		targetPath = "podcast-server.yaml"
	}
	
	// We need to set the config type to yaml so SafeWriteConfigAs knows what to do if extension is missing
	v.SetConfigType("yaml")

	if err := v.SafeWriteConfigAs(targetPath); err != nil {
		log.Fatalf("Error generating config file at %s: %v", targetPath, err)
	}
	log.Printf("Default configuration generated at: %s", targetPath)
}

// createPodcastFeed scans the audio directory and builds the RSS feed object.
func createPodcastFeed() *podcast.Podcast {
	// 1. Create the main channel object using config values
	now := time.Now().UTC()

	// Use the correct 5-argument signature: (title, link, description, pubDate, lastBuildDate)
	p := podcast.New(
		cfg.Podcast.Title,
		cfg.Podcast.Link,
		cfg.Podcast.Description,
		&now, // pubDate: *time.Time
		&now, // lastBuildDate: *time.Time
	)

	// Set other necessary fields
	p.Language = cfg.Podcast.Language
	p.IAuthor = cfg.Podcast.Author
	p.IExplicit = cfg.Podcast.Explicit
	p.Copyright = cfg.Podcast.Copyright // It is Copyright, not ICopyright
	// p.Type is not supported by this library directly

	
	if cfg.Podcast.OwnerName != "" || cfg.Podcast.OwnerEmail != "" {
		p.IOwner = &podcast.Author{
			Name:  cfg.Podcast.OwnerName,
			Email: cfg.Podcast.OwnerEmail,
		}
	} else {
		// Fallback to main author/email if owner not specified
		p.IOwner = &podcast.Author{
			Name:  cfg.Podcast.Author,
			Email: cfg.Podcast.Email,
		}
	}

	// FIX 1: Correctly reference the SubCategory field from the nested struct
	p.AddCategory(cfg.Podcast.Category, []string{cfg.Podcast.SubCategory})

	// Add Podcast Image
	// Priority: 1. Config Image URL, 2. Local "default" image
	var defaultImage string
	if cfg.Podcast.Image != "" {
		p.AddImage(cfg.Podcast.Image)
	} else {
		defaultImage = findImage("default", cfg.AudioFolder)
		if defaultImage != "" {
			p.AddImage(cfg.BaseURL + "/episodes/" + defaultImage)
		}
	}

	// 2. Scan the audio folder for files
	files, err := os.ReadDir(cfg.AudioFolder)
	if err != nil {
		log.Fatalf("Error reading audio directory %s: %v", cfg.AudioFolder, err)
	}

	var episodes []*podcast.Item

	for _, file := range files {
		if file.IsDir() || !isAudioFile(file.Name()) {
			continue
		}

		filePath := filepath.Join(cfg.AudioFolder, file.Name())

		info, err := file.Info()
		if err != nil {
			log.Printf("Error getting file info for %s: %v", file.Name(), err)
			continue
		}

		duration, err := getAudioDuration(filePath)
		if err != nil {
			log.Printf("Error calculating duration for %s: %v. Skipping file.", file.Name(), err)
			continue
		}

		baseName := strings.TrimSuffix(file.Name(), filepath.Ext(file.Name()))
		mdPath := filepath.Join(cfg.AudioFolder, baseName+".md")

		episodeTitle := baseName
		episodeDescription := fmt.Sprintf("A podcast episode named %s.", episodeTitle)

		if _, err := os.Stat(mdPath); err == nil {
			// Markdown file exists, so we parse it
			episodeTitle, episodeDescription = parseMarkdown(mdPath, baseName)
		}

		// Fallback to original title if markdown parsing fails to find one
		pubDate := info.ModTime()
		fileSize := info.Size()

		fileURL := cfg.BaseURL + "/episodes/" + file.Name()
		durationStr := formatDuration(duration)
		enclosureType := getEnclosureType(file.Name())

		item := podcast.Item{
			Title:       episodeTitle,
			Description: episodeDescription,
			PubDate:     &pubDate,
			Enclosure: &podcast.Enclosure{
				URL:    fileURL,
				Type:   enclosureType,
				Length: fileSize,
			},
			IDuration: durationStr,
			IExplicit: cfg.Podcast.Explicit,
		}

		// Add Episode Image
		img := findImage(baseName, cfg.AudioFolder)
		if img == "" {
			img = defaultImage
		}
		if img != "" {
			item.AddImage(cfg.BaseURL + "/episodes/" + img)
		}

		episodes = append(episodes, &item)
	}

	// Load sort order if custom sort is selected
	var sortOrder map[string]int
	if cfg.SortMethod == "custom" {
		sortOrder = loadSortOrder(filepath.Join(cfg.AudioFolder, "sort_order.txt"))
	}

	sort.Slice(episodes, func(i, j int) bool {
		switch cfg.SortMethod {
		case "date":
			// Newest first (descending)
			return episodes[i].PubDate.After(*episodes[j].PubDate)
		case "custom":
			// Extract filenames from URL to match against sort_order.txt
			// URL format: .../episodes/filename.mp3
			file1 := filepath.Base(episodes[i].Enclosure.URL)
			file2 := filepath.Base(episodes[j].Enclosure.URL)
			
			idx1, ok1 := sortOrder[file1]
			idx2, ok2 := sortOrder[file2]
			
			if ok1 && ok2 {
				return idx1 < idx2
			}
			if ok1 {
				return true // 1 is in list, 2 is not -> 1 comes first
			}
			if ok2 {
				return false // 2 is in list, 1 is not -> 2 comes first
			}
			// Fallback to filename sort for unlisted files
			return episodes[i].Title < episodes[j].Title
		default:
			// Default to filename (Title) sort (A-Z)
			return episodes[i].Title < episodes[j].Title
		}
	})

	for _, item := range episodes {
		if _, err := p.AddItem(*item); err != nil {
			log.Printf("Error adding item %s: %v", item.Title, err)
		}
	}

	return &p
}

// parseMarkdown reads a markdown file to extract a title (from the first H1 header)
// and a description (the rest of the content).
func parseMarkdown(filePath, fallbackTitle string) (title, description string) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		log.Printf("Error reading markdown file %s: %v", filePath, err)
		return fallbackTitle, fmt.Sprintf("A podcast episode named %s.", fallbackTitle)
	}

	lines := strings.Split(string(content), "\n")
	var titleFound bool
	var descLines []string

	for _, line := range lines {
		if !titleFound && strings.HasPrefix(line, "# ") {
			title = strings.TrimSpace(strings.TrimPrefix(line, "# "))
			titleFound = true
		} else {
			descLines = append(descLines, line)
		}
	}

	description = strings.TrimSpace(strings.Join(descLines, "\n"))
	return title, description
}

// formatDuration converts a time.Duration into the required HH:MM:SS string format.
func formatDuration(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}

// getAudioDuration calculates the duration of an MP3 file.
func getAudioDuration(filePath string) (time.Duration, error) {
	if strings.HasSuffix(strings.ToLower(filePath), ".m4a") {
		return getM4ADuration(filePath)
	}

	if !strings.HasSuffix(strings.ToLower(filePath), ".mp3") {
		return 0, nil
	}

	r, err := os.Open(filePath)
	if err != nil {
		return 0, err
	}
	defer r.Close()

	d := mp3.NewDecoder(r)
	var f mp3.Frame
	var skipped int

	var total time.Duration
	for {
		if err := d.Decode(&f, &skipped); err != nil {
			if err == io.EOF {
				break
			}
			return 0, fmt.Errorf("mp3 decode error: %w", err)
		}
		total += f.Duration()
	}

	return total, nil
}

// getM4ADuration calculates the duration of an M4A/MP4 file using dhowden/tag.
func getM4ADuration(filePath string) (time.Duration, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return 0, err
	}
	defer file.Close()



	// tag library doesn't always expose duration directly in the common interface
	// but for MP4 it might be hidden or we might need to check the specific type.
	// Wait, dhowden/tag's Metadata interface doesn't have Duration().
	// Let's check if we can get it.
	// Actually, dhowden/tag is mostly for ID3/metadata tags (Title, Artist, etc).
	// It might NOT support duration for all formats.
	
	// Let's double check if there is a simpler library for duration specifically.
	// "github.com/tcolgate/mp3" is used for MP3.
	
	// Re-evaluating: abema/go-mp4 IS the robust way, but verbose.
	// Let's try to make the abema code correct and concise instead of switching to a library that might not support duration.
	// OR use "github.com/hajimehoshi/go-mp3" (no, that's mp3)
	// "github.com/faiface/beep" (audio playing, might be heavy)
	
	// Let's stick to abema/go-mp4 but FIX the implementation to be correct.
	// The previous error was because I used the wrong API.
	// I will fix the abema implementation.
	
	// We need to use mp4.ReadBoxStructure correctly.
	// The signature is: func ReadBoxStructure(r io.ReadSeeker, h ReadHandler) ([]interface{}, error)
	
	_, err = mp4.ReadBoxStructure(file, func(h *mp4.ReadHandle) (interface{}, error) {
		if h.BoxInfo.Type == mp4.BoxTypeMvhd() {
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			return box, nil
		}
		// Expand all other boxes to find mvhd nested in moov
		return h.Expand()
	})
	
	// Wait, ReadBoxStructure returns the result of the handler.
	// We need to capture the duration from the closure.
	
	var duration time.Duration
	var found bool
	
	_, err = mp4.ReadBoxStructure(file, func(h *mp4.ReadHandle) (interface{}, error) {
		if h.BoxInfo.Type == mp4.BoxTypeMvhd() {
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			
			if mvhd, ok := box.(*mp4.Mvhd); ok {
				var d uint64
				if mvhd.Version == 1 {
					d = mvhd.DurationV1
				} else {
					d = uint64(mvhd.DurationV0)
				}
				
				seconds := float64(d) / float64(mvhd.Timescale)
				duration = time.Duration(seconds * float64(time.Second))
				found = true
				// We found it, we can stop or just return. 
				// To stop early we might return an error or just let it finish (it's fast).
			}
			return nil, nil
		}
		
		if h.BoxInfo.Type == mp4.BoxTypeMoov() {
			return h.Expand()
		}
		
		return nil, nil
	})
	
	if err != nil {
		return 0, err
	}
	
	if !found {
		return 0, fmt.Errorf("mvhd box not found")
	}
	
	return duration, nil
}

// findImage looks for an image file with the given base name and common extensions.
func findImage(baseName, folder string) string {
	extensions := []string{".jpg", ".jpeg", ".png"}
	for _, ext := range extensions {
		fileName := baseName + ext
		path := filepath.Join(folder, fileName)
		if _, err := os.Stat(path); err == nil {
			return fileName
		}
	}
	return ""
}

// isAudioFile is a simple extension check
func isAudioFile(filename string) bool {
	ext := strings.ToLower(filepath.Ext(filename))
	return ext == ".mp3" || ext == ".m4a"
}

// getMimeType returns the correct MIME type string based on the file extension.
func getMimeType(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".mp3":
		return "audio/mpeg"
	case ".m4a":
		return "audio/x-m4a"
	default:
		return "application/octet-stream"
	}
}

// getEnclosureType returns the correct podcast.EnclosureType based on the file extension.
func getEnclosureType(filename string) podcast.EnclosureType {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".mp3":
		return podcast.MP3
	case ".m4a":
		return podcast.M4A
	default:
		return 99 // An invalid type that will be caught by AddItem validation
	}
}

// watchAudioFolder periodically updates the feed.
func watchAudioFolder() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	log.Printf("Feed auto-update enabled (every 5 minutes)")

	for range ticker.C {
		log.Println("Updating podcast feed...")
		newFeed := createPodcastFeed()
		feedManager.Update(newFeed)
		log.Println("Podcast feed updated.")
	}
}

// loadSortOrder reads the sort_order.txt file and returns a map of filename -> index.
func loadSortOrder(path string) map[string]int {
	order := make(map[string]int)
	content, err := os.ReadFile(path)
	if err != nil {
		// If file doesn't exist or error reading, return empty map (will fallback to filename sort)
		if !os.IsNotExist(err) {
			log.Printf("Error reading sort_order.txt: %v", err)
		}
		return order
	}

	lines := strings.Split(string(content), "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			order[trimmed] = i
		}
	}
	return order
}

// EpisodeData represents an episode for the HTML template
type EpisodeData struct {
	Title       string
	Description template.HTML
	PubDate     string
	Duration    string
	AudioURL    string
	ImageURL    string
}

// IndexPageData represents the data for the index page template
type IndexPageData struct {
	PodcastTitle       string
	PodcastDescription string
	PodcastImage       string
	FeedURL            string
	Episodes           []EpisodeData
	CurrentPage        int
	TotalPages         int
	TotalEpisodes      int
	PageNumbers        []int
}

// serveIndexPage handles the root path and renders the HTML landing page
func serveIndexPage(w http.ResponseWriter, r *http.Request) {
	// Parse template with custom functions
	funcMap := template.FuncMap{
		"sub": func(a, b int) int { return a - b },
		"add": func(a, b int) int { return a + b },
	}
	
	tmpl, err := template.New("index.html").Funcs(funcMap).ParseFiles("templates/index.html")
	if err != nil {
		log.Printf("Error parsing template: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// Get current feed
	currentFeed := feedManager.Get()
	if currentFeed == nil {
		http.Error(w, "Feed not ready", http.StatusServiceUnavailable)
		return
	}

	// Extract episodes from feed
	var episodes []EpisodeData
	for _, item := range currentFeed.Items {
		pubDate := ""
		if item.PubDate != nil {
			pubDate = item.PubDate.Format("Jan 2, 2006")
		}

		imageURL := ""
		if item.IImage != nil {
			imageURL = item.IImage.HREF
		}

		audioURL := ""
		if item.Enclosure != nil {
			audioURL = item.Enclosure.URL
		}

		// Convert markdown description to HTML
		extensions := parser.CommonExtensions | parser.Autolink
		p := parser.NewWithExtensions(extensions)
		doc := p.Parse([]byte(item.Description))

		htmlFlags := html.CommonFlags | html.HrefTargetBlank
		opts := html.RendererOptions{Flags: htmlFlags}
		renderer := html.NewRenderer(opts)

		htmlContent := markdown.Render(doc, renderer)

		episodes = append(episodes, EpisodeData{
			Title:       item.Title,
			Description: template.HTML(htmlContent),
			PubDate:     pubDate,
			Duration:    item.IDuration,
			AudioURL:    audioURL,
			ImageURL:    imageURL,
		})
	}

	// Pagination
	const episodesPerPage = 10
	page := 1
	if pageStr := r.URL.Query().Get("page"); pageStr != "" {
		if p, err := fmt.Sscanf(pageStr, "%d", &page); err == nil && p == 1 {
			if page < 1 {
				page = 1
			}
		}
	}

	totalEpisodes := len(episodes)
	totalPages := (totalEpisodes + episodesPerPage - 1) / episodesPerPage
	if totalPages < 1 {
		totalPages = 1
	}
	if page > totalPages {
		page = totalPages
	}

	// Calculate pagination range
	start := (page - 1) * episodesPerPage
	end := start + episodesPerPage
	if end > totalEpisodes {
		end = totalEpisodes
	}

	var paginatedEpisodes []EpisodeData
	if totalEpisodes > 0 {
		paginatedEpisodes = episodes[start:end]
	}

	// Generate page numbers for pagination (show max 7 pages)
	pageNumbers := generatePageNumbers(page, totalPages)

	// Get podcast image
	podcastImage := ""
	if currentFeed.IImage != nil {
		podcastImage = currentFeed.IImage.HREF
	}

	data := IndexPageData{
		PodcastTitle:       currentFeed.Title,
		PodcastDescription: currentFeed.Description,
		PodcastImage:       podcastImage,
		FeedURL:            cfg.Podcast.Link,
		Episodes:           paginatedEpisodes,
		CurrentPage:        page,
		TotalPages:         totalPages,
		TotalEpisodes:      totalEpisodes,
		PageNumbers:        pageNumbers,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, data); err != nil {
		log.Printf("Error executing template: %v", err)
	}
}

// generatePageNumbers creates a slice of page numbers for pagination display
// Shows up to 7 pages with current page in the middle when possible
func generatePageNumbers(currentPage, totalPages int) []int {
	if totalPages <= 7 {
		pages := make([]int, totalPages)
		for i := range pages {
			pages[i] = i + 1
		}
		return pages
	}

	// Show 7 pages max
	var pages []int
	start := currentPage - 3
	end := currentPage + 3

	if start < 1 {
		start = 1
		end = 7
	}
	if end > totalPages {
		end = totalPages
		start = totalPages - 6
	}

	for i := start; i <= end; i++ {
		pages = append(pages, i)
	}
	return pages
}

// ensureTemplateExists checks if templates/index.html exists, and if not, creates it.
func ensureTemplateExists() {
	templatePath := filepath.Join("templates", "index.html")
	if _, err := os.Stat(templatePath); os.IsNotExist(err) {
		log.Println("Template not found. Generating default templates/index.html...")
		
		if err := os.MkdirAll("templates", 0755); err != nil {
			log.Fatalf("Error creating templates directory: %v", err)
		}

		if err := os.WriteFile(templatePath, []byte(defaultIndexHTML), 0644); err != nil {
			log.Fatalf("Error writing default template: %v", err)
		}
		log.Println("Default template generated successfully.")
	}
}

const defaultIndexHTML = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>{{.PodcastTitle}}</title>
    <meta name="description" content="{{.PodcastDescription}}">
    <link rel="alternate" type="application/rss+xml" title="{{.PodcastTitle}}" href="{{.FeedURL}}">
    <link rel="preconnect" href="https://fonts.googleapis.com">
    <link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
    <link href="https://fonts.googleapis.com/css2?family=Inter:wght@300;400;500;600;700&display=swap" rel="stylesheet">
    <style>
        * {
            margin: 0;
            padding: 0;
            box-sizing: border-box;
        }

        :root {
            --primary: hsl(260, 85%, 60%);
            --primary-dark: hsl(260, 85%, 50%);
            --primary-light: hsl(260, 85%, 70%);
            --secondary: hsl(200, 90%, 55%);
            --bg-dark: hsl(230, 25%, 8%);
            --bg-card: hsl(230, 20%, 12%);
            --bg-card-hover: hsl(230, 20%, 15%);
            --text-primary: hsl(0, 0%, 98%);
            --text-secondary: hsl(0, 0%, 70%);
            --text-muted: hsl(0, 0%, 50%);
            --border: hsl(230, 15%, 20%);
            --shadow: 0 8px 32px rgba(0, 0, 0, 0.4);
            --shadow-lg: 0 16px 48px rgba(0, 0, 0, 0.6);
        }

        body {
            font-family: 'Inter', -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif;
            background: linear-gradient(135deg, var(--bg-dark) 0%, hsl(240, 25%, 10%) 100%);
            color: var(--text-primary);
            min-height: 100vh;
            line-height: 1.6;
        }

        .container {
            max-width: 1200px;
            margin: 0 auto;
            padding: 2rem;
        }

        /* Header */
        header {
            text-align: center;
            padding: 3rem 0 2rem;
            background: linear-gradient(180deg, rgba(138, 43, 226, 0.1) 0%, transparent 100%);
            border-bottom: 1px solid var(--border);
            margin-bottom: 3rem;
        }

        .podcast-image {
            width: 200px;
            height: 200px;
            border-radius: 24px;
            margin: 0 auto 1.5rem;
            box-shadow: var(--shadow-lg);
            object-fit: cover;
            border: 3px solid var(--border);
            transition: transform 0.3s ease;
        }

        .podcast-image:hover {
            transform: scale(1.05);
        }

        h1 {
            font-size: 3rem;
            font-weight: 700;
            margin-bottom: 0.5rem;
            background: linear-gradient(135deg, var(--primary-light), var(--secondary));
            -webkit-background-clip: text;
            -webkit-text-fill-color: transparent;
            background-clip: text;
        }

        .podcast-description {
            color: var(--text-secondary);
            font-size: 1.1rem;
            max-width: 700px;
            margin: 0 auto 1.5rem;
        }

        .header-actions {
            display: flex;
            gap: 1rem;
            justify-content: center;
            flex-wrap: wrap;
        }

        .btn {
            padding: 0.75rem 1.5rem;
            border-radius: 12px;
            font-weight: 600;
            text-decoration: none;
            transition: all 0.3s ease;
            border: none;
            cursor: pointer;
            font-size: 1rem;
            display: inline-flex;
            align-items: center;
            gap: 0.5rem;
        }

        .btn-primary {
            background: linear-gradient(135deg, var(--primary), var(--primary-dark));
            color: white;
            box-shadow: 0 4px 16px rgba(138, 43, 226, 0.4);
        }

        .btn-primary:hover {
            transform: translateY(-2px);
            box-shadow: 0 6px 24px rgba(138, 43, 226, 0.6);
        }

        .btn-secondary {
            background: var(--bg-card);
            color: var(--text-primary);
            border: 1px solid var(--border);
        }

        .btn-secondary:hover {
            background: var(--bg-card-hover);
            border-color: var(--primary);
        }

        /* Episodes Grid */
        .episodes-header {
            display: flex;
            justify-content: space-between;
            align-items: center;
            margin-bottom: 2rem;
        }

        .episodes-header h2 {
            font-size: 2rem;
            font-weight: 600;
        }

        .episode-count {
            color: var(--text-muted);
            font-size: 0.95rem;
        }

        .episodes-grid {
            display: grid;
            gap: 1.5rem;
            margin-bottom: 3rem;
        }

        .episode-card {
            background: var(--bg-card);
            border: 1px solid var(--border);
            border-radius: 16px;
            padding: 1.5rem;
            transition: all 0.3s ease;
            cursor: pointer;
            position: relative;
            overflow: hidden;
        }

        .episode-card::before {
            content: '';
            position: absolute;
            top: 0;
            left: 0;
            right: 0;
            height: 3px;
            background: linear-gradient(90deg, var(--primary), var(--secondary));
            transform: scaleX(0);
            transition: transform 0.3s ease;
        }

        .episode-card:hover {
            background: var(--bg-card-hover);
            border-color: var(--primary);
            transform: translateY(-4px);
            box-shadow: var(--shadow);
        }

        .episode-card:hover::before {
            transform: scaleX(1);
        }

        .episode-header {
            display: flex;
            gap: 1.5rem;
            margin-bottom: 1rem;
        }

        .episode-image {
            width: 120px;
            height: 120px;
            border-radius: 12px;
            object-fit: cover;
            flex-shrink: 0;
            border: 2px solid var(--border);
        }

        .episode-info {
            flex: 1;
            min-width: 0;
        }

        .episode-title {
            font-size: 1.4rem;
            font-weight: 600;
            margin-bottom: 0.5rem;
            color: var(--text-primary);
        }

        .episode-meta {
            display: flex;
            gap: 1.5rem;
            flex-wrap: wrap;
            color: var(--text-muted);
            font-size: 0.9rem;
            margin-bottom: 0.75rem;
        }

        .episode-meta span {
            display: flex;
            align-items: center;
            gap: 0.4rem;
        }

        .episode-description {
            color: var(--text-secondary);
            line-height: 1.7;
            margin-bottom: 1rem;
            display: -webkit-box;
            -webkit-line-clamp: 3;
            line-clamp: 3;
            -webkit-box-orient: vertical;
            overflow: hidden;
        }

        .episode-description.expanded {
            display: block;
            -webkit-line-clamp: unset;
            line-clamp: unset;
        }

        /* Markdown Content Styles */
        .episode-description p {
            margin-bottom: 1rem;
        }

        .episode-description ul, 
        .episode-description ol {
            margin-bottom: 1rem;
            padding-left: 1.5rem;
        }

        .episode-description li {
            margin-bottom: 0.5rem;
        }

        .episode-description a {
            color: var(--primary-light);
            text-decoration: none;
            border-bottom: 1px solid transparent;
            transition: border-color 0.3s ease;
        }

        .episode-description a:hover {
            border-bottom-color: var(--primary-light);
        }

        .episode-description code {
            background: rgba(255, 255, 255, 0.1);
            padding: 0.2rem 0.4rem;
            border-radius: 4px;
            font-family: monospace;
            font-size: 0.9em;
        }

        .episode-description blockquote {
            border-left: 4px solid var(--primary);
            padding-left: 1rem;
            font-style: italic;
            color: var(--text-muted);
            margin: 1rem 0;
        }

        .episode-actions {
            display: flex;
            gap: 1rem;
            align-items: center;
        }

        .play-btn {
            background: linear-gradient(135deg, var(--primary), var(--primary-dark));
            color: white;
            border: none;
            padding: 0.6rem 1.5rem;
            border-radius: 8px;
            font-weight: 600;
            cursor: pointer;
            transition: all 0.3s ease;
            display: flex;
            align-items: center;
            gap: 0.5rem;
        }

        .play-btn:hover {
            transform: scale(1.05);
            box-shadow: 0 4px 16px rgba(138, 43, 226, 0.5);
        }

        .expand-btn {
            background: transparent;
            color: var(--text-muted);
            border: none;
            cursor: pointer;
            font-size: 0.9rem;
            text-decoration: underline;
            transition: color 0.3s ease;
        }

        .expand-btn:hover {
            color: var(--primary-light);
        }

        /* Audio Player */
        .audio-player {
            position: fixed;
            bottom: 0;
            left: 0;
            right: 0;
            background: linear-gradient(180deg, var(--bg-card) 0%, hsl(230, 20%, 10%) 100%);
            border-top: 1px solid var(--border);
            padding: 1.5rem;
            box-shadow: 0 -8px 32px rgba(0, 0, 0, 0.6);
            transform: translateY(100%);
            transition: transform 0.4s cubic-bezier(0.4, 0, 0.2, 1);
            z-index: 1000;
            backdrop-filter: blur(20px);
        }

        .audio-player.active {
            transform: translateY(0);
        }

        .player-content {
            max-width: 1200px;
            margin: 0 auto;
            display: flex;
            gap: 1.5rem;
            align-items: center;
        }

        .player-image {
            width: 80px;
            height: 80px;
            border-radius: 12px;
            object-fit: cover;
            flex-shrink: 0;
            box-shadow: 0 4px 16px rgba(0, 0, 0, 0.4);
        }

        .player-info {
            flex: 1;
            min-width: 0;
        }

        .player-title {
            font-weight: 600;
            font-size: 1.1rem;
            margin-bottom: 0.25rem;
            white-space: nowrap;
            overflow: hidden;
            text-overflow: ellipsis;
        }

        .player-controls {
            display: flex;
            gap: 1rem;
            align-items: center;
            margin-top: 0.5rem;
        }

        .control-btn {
            background: transparent;
            border: none;
            color: var(--text-primary);
            cursor: pointer;
            padding: 0.5rem;
            border-radius: 50%;
            transition: all 0.3s ease;
            display: flex;
            align-items: center;
            justify-content: center;
        }

        .control-btn:hover {
            background: var(--bg-card-hover);
            color: var(--primary-light);
        }

        .control-btn.play-pause {
            background: var(--primary);
            width: 48px;
            height: 48px;
        }

        .control-btn.play-pause:hover {
            background: var(--primary-dark);
            transform: scale(1.1);
        }

        .progress-container {
            flex: 1;
            display: flex;
            gap: 1rem;
            align-items: center;
        }

        .progress-bar {
            flex: 1;
            height: 6px;
            background: var(--border);
            border-radius: 3px;
            cursor: pointer;
            position: relative;
            overflow: hidden;
        }

        .progress-bar::before {
            content: '';
            position: absolute;
            top: 0;
            left: 0;
            height: 100%;
            background: linear-gradient(90deg, var(--primary), var(--secondary));
            width: var(--progress, 0%);
            transition: width 0.1s linear;
        }

        .time {
            color: var(--text-muted);
            font-size: 0.85rem;
            font-variant-numeric: tabular-nums;
            min-width: 45px;
        }

        .close-player {
            background: transparent;
            border: none;
            color: var(--text-muted);
            cursor: pointer;
            padding: 0.5rem;
            transition: color 0.3s ease;
        }

        .close-player:hover {
            color: var(--text-primary);
        }

        /* Pagination */
        .pagination {
            display: flex;
            justify-content: center;
            gap: 0.5rem;
            margin: 3rem 0;
        }

        .page-btn {
            padding: 0.6rem 1rem;
            background: var(--bg-card);
            border: 1px solid var(--border);
            border-radius: 8px;
            color: var(--text-primary);
            cursor: pointer;
            transition: all 0.3s ease;
            font-weight: 500;
        }

        .page-btn:hover:not(:disabled) {
            background: var(--bg-card-hover);
            border-color: var(--primary);
            transform: translateY(-2px);
        }

        .page-btn.active {
            background: var(--primary);
            border-color: var(--primary);
        }

        .page-btn:disabled {
            opacity: 0.4;
            cursor: not-allowed;
        }

        /* Icons (using Unicode symbols) */
        .icon-play::before { content: '▶'; }
        .icon-pause::before { content: '⏸'; }
        .icon-calendar::before { content: '📅'; }
        .icon-clock::before { content: '⏱'; }
        .icon-rss::before { content: '📡'; }
        .icon-close::before { content: '✕'; }
        .icon-skip-back::before { content: '⏮'; }
        .icon-skip-forward::before { content: '⏭'; }

        /* Responsive */
        @media (max-width: 768px) {
            h1 {
                font-size: 2rem;
            }

            .episode-header {
                flex-direction: column;
            }

            .episode-image {
                width: 100%;
                height: 200px;
            }

            .player-content {
                flex-wrap: wrap;
            }

            .player-controls {
                width: 100%;
            }
        }

        /* Loading state */
        .loading {
            text-align: center;
            padding: 3rem;
            color: var(--text-muted);
        }

        /* Empty state */
        .empty-state {
            text-align: center;
            padding: 4rem 2rem;
            color: var(--text-muted);
        }

        .empty-state h3 {
            font-size: 1.5rem;
            margin-bottom: 0.5rem;
            color: var(--text-secondary);
        }
    </style>
</head>
<body>
    <header>
        <div class="container">
            {{if .PodcastImage}}
            <img src="{{.PodcastImage}}" alt="{{.PodcastTitle}}" class="podcast-image">
            {{end}}
            <h1>{{.PodcastTitle}}</h1>
            <p class="podcast-description">{{.PodcastDescription}}</p>
            <div class="header-actions">
                <a href="{{.FeedURL}}" class="btn btn-primary">
                    <span class="icon-rss"></span>
                    Subscribe via RSS
                </a>
                <a href="#episodes" class="btn btn-secondary">Browse Episodes</a>
            </div>
        </div>
    </header>

    <main class="container">
        <div class="episodes-header" id="episodes">
            <div>
                <h2>Episodes</h2>
                <p class="episode-count">{{.TotalEpisodes}} episode{{if ne .TotalEpisodes 1}}s{{end}} available</p>
            </div>
        </div>

        {{if .Episodes}}
        <div class="episodes-grid">
            {{range .Episodes}}
            <div class="episode-card" data-episode-url="{{.AudioURL}}">
                <div class="episode-header">
                    {{if .ImageURL}}
                    <img src="{{.ImageURL}}" alt="{{.Title}}" class="episode-image">
                    {{end}}
                    <div class="episode-info">
                        <h3 class="episode-title">{{.Title}}</h3>
                        <div class="episode-meta">
                            <span><span class="icon-calendar"></span> {{.PubDate}}</span>
                            <span><span class="icon-clock"></span> {{.Duration}}</span>
                        </div>
                    </div>
                </div>
                {{if .Description}}
                <div class="episode-description">{{.Description}}</div>
                {{end}}
                <div class="episode-actions">
                    <button class="play-btn" onclick="playEpisode('{{.AudioURL}}', '{{.Title}}', '{{.ImageURL}}', event)">
                        <span class="icon-play"></span>
                        Play Episode
                    </button>
                    {{if .Description}}
                    <button class="expand-btn" onclick="toggleDescription(event)">Show more</button>
                    {{end}}
                </div>
            </div>
            {{end}}
        </div>

        {{if gt .TotalPages 1}}
        <div class="pagination">
            {{if gt .CurrentPage 1}}
            <a href="?page={{sub .CurrentPage 1}}" class="page-btn">Previous</a>
            {{else}}
            <button class="page-btn" disabled>Previous</button>
            {{end}}

            {{range .PageNumbers}}
            {{if eq . $.CurrentPage}}
            <a href="?page={{.}}" class="page-btn active">{{.}}</a>
            {{else}}
            <a href="?page={{.}}" class="page-btn">{{.}}</a>
            {{end}}
            {{end}}

            {{if lt .CurrentPage .TotalPages}}
            <a href="?page={{add .CurrentPage 1}}" class="page-btn">Next</a>
            {{else}}
            <button class="page-btn" disabled>Next</button>
            {{end}}
        </div>
        {{end}}
        {{else}}
        <div class="empty-state">
            <h3>No Episodes Yet</h3>
            <p>Add audio files to the episodes folder to get started.</p>
        </div>
        {{end}}
    </main>

    <!-- Audio Player -->
    <div class="audio-player" id="audioPlayer">
        <div class="player-content">
            <img src="" alt="" class="player-image" id="playerImage">
            <div class="player-info">
                <div class="player-title" id="playerTitle">No episode playing</div>
                <div class="player-controls">
                    <button class="control-btn play-pause" id="playPauseBtn" onclick="togglePlayPause()">
                        <span class="icon-play"></span>
                    </button>
                    <div class="progress-container">
                        <span class="time" id="currentTime">0:00</span>
                        <div class="progress-bar" id="progressBar" onclick="seek(event)"></div>
                        <span class="time" id="duration">0:00</span>
                    </div>
                </div>
            </div>
            <button class="close-player" onclick="closePlayer()">
                <span class="icon-close"></span>
            </button>
        </div>
        <audio id="audioElement"></audio>
    </div>

    <script>
        const audioElement = document.getElementById('audioElement');
        const audioPlayer = document.getElementById('audioPlayer');
        const playerTitle = document.getElementById('playerTitle');
        const playerImage = document.getElementById('playerImage');
        const playPauseBtn = document.getElementById('playPauseBtn');
        const progressBar = document.getElementById('progressBar');
        const currentTimeEl = document.getElementById('currentTime');
        const durationEl = document.getElementById('duration');

        function playEpisode(url, title, imageUrl, event) {
            event.stopPropagation();
            audioElement.src = url;
            playerTitle.textContent = title;
            playerImage.src = imageUrl || '';
            audioPlayer.classList.add('active');
            audioElement.play();
            updatePlayPauseButton();
        }

        function togglePlayPause() {
            if (audioElement.paused) {
                audioElement.play();
            } else {
                audioElement.pause();
            }
            updatePlayPauseButton();
        }

        function updatePlayPauseButton() {
            const icon = playPauseBtn.querySelector('span');
            if (audioElement.paused) {
                icon.className = 'icon-play';
            } else {
                icon.className = 'icon-pause';
            }
        }

        function closePlayer() {
            audioPlayer.classList.remove('active');
            audioElement.pause();
            audioElement.src = '';
        }

        function seek(event) {
            const rect = progressBar.getBoundingClientRect();
            const percent = (event.clientX - rect.left) / rect.width;
            audioElement.currentTime = percent * audioElement.duration;
        }

        function formatTime(seconds) {
            if (isNaN(seconds)) return '0:00';
            const mins = Math.floor(seconds / 60);
            const secs = Math.floor(seconds % 60);
            return ` + "`" + `${mins}:${secs.toString().padStart(2, '0')}` + "`" + `;
        }

        audioElement.addEventListener('timeupdate', () => {
            const progress = (audioElement.currentTime / audioElement.duration) * 100;
            progressBar.style.setProperty('--progress', ` + "`" + `${progress}%` + "`" + `);
            currentTimeEl.textContent = formatTime(audioElement.currentTime);
        });

        audioElement.addEventListener('loadedmetadata', () => {
            durationEl.textContent = formatTime(audioElement.duration);
        });

        audioElement.addEventListener('play', updatePlayPauseButton);
        audioElement.addEventListener('pause', updatePlayPauseButton);

        function toggleDescription(event) {
            event.stopPropagation();
            const card = event.target.closest('.episode-card');
            const description = card.querySelector('.episode-description');
            const btn = event.target;
            
            description.classList.toggle('expanded');
            btn.textContent = description.classList.contains('expanded') ? 'Show less' : 'Show more';
        }

        // Keyboard shortcuts
        document.addEventListener('keydown', (e) => {
            if (e.code === 'Space' && audioPlayer.classList.contains('active')) {
                e.preventDefault();
                togglePlayPause();
            }
        });
    </script>
</body>
</html>`


