package main

import (
	"context"
	"flag"
	"fmt"
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

	// Override audio folder if flag is provided
	if *audioPath != "" {
		cfg.AudioFolder = *audioPath
	}
	
	// Override port if flag is provided
	if *portFlag != "" {
		cfg.Port = *portFlag
	}
	
	// Override base URL if flag is provided
	if *baseUrlFlag != "" {
		cfg.BaseURL = *baseUrlFlag
	}
	
	// Override sort method if flag is provided (or use default)
	cfg.SortMethod = *sortMethod
	
	// Ensure audio folder defaults to ./audio if empty
	if cfg.AudioFolder == "" {
		cfg.AudioFolder = "./audio"
	}

	var listener net.Listener

	if *useTailscale {
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
		if *verbose {
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

	if !*useTailscale {
		log.Printf("Podcast Server running at %s", cfg.BaseURL)
		log.Printf("Listening on port %s", serverPort)
		log.Printf("RSS Feed URL: %s", cfg.Podcast.Link)
	}
	log.Printf("Serving files from local directory: %s", cfg.AudioFolder)

	// Start the server
	if *useTailscale {
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
