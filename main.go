package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/calendar/v3"
	"gopkg.in/yaml.v3"
)

type appConfig struct {
	RedirectURL           string `yaml:"redirect_url"`
	SourceCalendarID      string `yaml:"source_calendar_id"`
	DestinationCalendarID string `yaml:"destination_calendar_id"`
}

func loadConfig(path string) (*appConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c appConfig
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	if c.RedirectURL == "" {
		return nil, fmt.Errorf("%s: redirect_url is required", path)
	}
	if c.SourceCalendarID == "" {
		return nil, fmt.Errorf("%s: source_calendar_id is required", path)
	}
	if c.DestinationCalendarID == "" {
		return nil, fmt.Errorf("%s: destination_calendar_id is required", path)
	}
	return &c, nil
}

// tokenFromFile reads a token from a file.
func tokenFromFile(file string) (*oauth2.Token, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tok := &oauth2.Token{}
	err = json.NewDecoder(f).Decode(tok)
	return tok, err
}

// getTokenFromWeb starts a Gin server with OAuth routes and returns a token.
// Does NOT override the RedirectURL in config; uses what’s in credentials.json.
func getTokenFromWeb(config *oauth2.Config) *oauth2.Token {
	stateBytes := make([]byte, 32)
	if _, err := rand.Read(stateBytes); err != nil {
		log.Fatal(err)
	}
	state := hex.EncodeToString(stateBytes)
	authURL := config.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce)
	codeCh := make(chan string, 1)

	r := gin.Default()
	r.GET("/setup/auth", func(c *gin.Context) {
		c.Redirect(http.StatusFound, authURL)
	})
	r.GET("/setup/auth/callback", func(c *gin.Context) {
		if c.Query("state") != state {
			c.String(http.StatusBadRequest, "Invalid OAuth state")
			return
		}
		if errParam := c.Query("error"); errParam != "" {
			c.String(http.StatusBadRequest, "Authentication failed: %s", errParam)
			codeCh <- ""
			return
		}
		code := c.Query("code")
		c.String(http.StatusOK, "Authorization received; you can close this window.")
		codeCh <- code
	})

	srv := &http.Server{Addr: "127.0.0.1:1853", Handler: r, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	fmt.Println("Open http://localhost:1853/setup/auth in your browser to authorize the app.")
	code := <-codeCh

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx)

	if code == "" {
		log.Fatalf("No code received; aborting.")
	}
	tok, err := config.Exchange(context.Background(), code)
	if err != nil {
		log.Fatalf("Token exchange error: %v", err)
	}
	return tok
}

// saveToken writes a token to a file.
func saveToken(path string, token *oauth2.Token) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		log.Fatalf("Unable to create %s: %v", path, err)
	}
	defer f.Close()
	json.NewEncoder(f).Encode(token)
}

func main() {
	configPath := flag.String("config", "config.yaml", "Config file")
	credentialsPath := flag.String("credentials", "credentials.json", "Google OAuth client JSON")
	tokenPath := flag.String("token", "token.json", "Google OAuth token JSON (read-only for sync)")
	authorize := flag.Bool("authorize", false, "Authorize interactively on localhost, save a token, and exit")
	dryRun := flag.Bool("dry-run", false, "Read calendars and report planned changes without writing")
	sinceFlag := flag.String("since", "", "Only sync source events updated since this RFC3339 timestamp")
	timeout := flag.Duration("timeout", 8*time.Minute, "Overall sync timeout")
	flag.Parse()
	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	if cfg.SourceCalendarID == cfg.DestinationCalendarID {
		log.Fatal("source and destination must differ")
	}
	creds, err := os.ReadFile(*credentialsPath)
	if err != nil {
		log.Fatalf("Read OAuth credentials: %v", err)
	}
	config, err := google.ConfigFromJSON(creds, calendar.CalendarScope)
	if err != nil {
		log.Fatal(err)
	}
	config.RedirectURL = cfg.RedirectURL
	if *authorize {
		saveToken(*tokenPath, getTokenFromWeb(config))
		return
	}
	tok, err := tokenFromFile(*tokenPath)
	if err != nil {
		log.Fatalf("Read token: %v; use --authorize locally first", err)
	}
	if tok.RefreshToken == "" {
		log.Fatal("Token has no refresh token; use --authorize locally first")
	}
	since := time.Time{}
	if *sinceFlag != "" {
		since, err = time.Parse(time.RFC3339, *sinceFlag)
		if err != nil {
			log.Fatalf("Bad --since timestamp: %v", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	client := config.Client(ctx, tok)
	client.Timeout = 45 * time.Second
	svc, err := calendar.New(client)
	if err != nil {
		log.Fatal(err)
	}
	stats, err := syncEvents(ctx, svc, cfg.SourceCalendarID, cfg.DestinationCalendarID, since, *dryRun)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("Sync completed (dry_run=%t): created=%d updated=%d unchanged=%d", *dryRun, stats.Created, stats.Updated, stats.Unchanged)
}
