package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

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
	} `mapstructure:"podcast"`
}

var cfg Config // Global to hold the configuration

func main() {
	useTailscale := flag.Bool("tailscale", false, "Enable Tailscale Funnel support")
	verbose := flag.Bool("verbose", false, "Enable verbose logging for Tailscale")
	flag.Parse()

	loadConfig()

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

	p := createPodcastFeed()

	// 1. Handler for the RSS feed
	http.HandleFunc("/"+cfg.FeedFileName, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		if err := p.Encode(w); err != nil {
			log.Printf("Error encoding feed: %v", err)
			http.Error(w, "Could not generate feed", http.StatusInternalServerError)
		}
	})

	// 2. Handler for the audio files
	audioHandler := http.StripPrefix("/episodes/", http.FileServer(http.Dir(cfg.AudioFolder)))
	http.Handle("/episodes/", audioHandler)

	// Logging based on configuration
	serverPort := ":" + cfg.Port
	if !*useTailscale {
		log.Printf("Podcast Server running at %s", cfg.BaseURL)
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
	v.SetDefault("port", "8080")
	v.SetDefault("base_url", "http://localhost:8080")
	v.SetDefault("audio_folder", "./audio")
	v.SetDefault("podcast.title", "My Golang Powered Podcast")
	v.SetDefault("podcast.description", "A podcast generated automatically from a folder of audio files.")
	v.SetDefault("podcast.author", "The Go Gopher")
	v.SetDefault("podcast.email", "gopher@golang.org")
	v.SetDefault("podcast.language", "en-us")
	v.SetDefault("podcast.category", "Technology")
	v.SetDefault("podcast.sub_category", "Software Development")
	v.SetDefault("podcast.explicit", "no")
}

// loadConfig initializes Viper, reads the configuration file,
// and if not found, generates a default configuration file and exits.
func loadConfig() {
	v := viper.New()
	setDefaults(v)

	v.SetConfigName("config")
	v.AddConfigPath(".")
	v.SetConfigType("yaml")

	v.SetEnvPrefix("PODCAST")
	v.AutomaticEnv()

	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			log.Println("Configuration file (config.yaml) not found. Generating default config.yaml.")

			defaultConfigFile := "config.yaml"
			if err := v.SafeWriteConfigAs(defaultConfigFile); err != nil {
				log.Fatalf("Fatal error generating default config file: %s", err)
			}

			log.Printf("Default configuration written to: %s.", defaultConfigFile)
			log.Printf("Please edit this file (especially 'base_url') and restart the application.")

			os.Exit(0)
		} else {
			log.Fatalf("Fatal error reading config file: %s", err)
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

	// FIX 1: Correctly reference the SubCategory field from the nested struct
	p.AddCategory(cfg.Podcast.Category, []string{cfg.Podcast.SubCategory})

	// Add Podcast Image
	defaultImage := findImage("default", cfg.AudioFolder)
	if defaultImage != "" {
		p.AddImage(cfg.BaseURL + "/episodes/" + defaultImage)
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

	sort.Slice(episodes, func(i, j int) bool {
		return episodes[i].Title < episodes[j].Title
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
