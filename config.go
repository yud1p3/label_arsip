package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// Config menampung seluruh setelan runtime aplikasi.
// Nilai dibaca dari environment OS, dengan fallback isian file .env.
type Config struct {
	OnlyOfficeURL  string // URL Document Server, mis. http://localhost:8026
	JWTSecret      string // secret JWT OnlyOffice (services.CoAuthoring.secret)
	JWTHeader      string // nama header pembawa token, mis. Authorization
	AppInternalURL string // URL aplikasi ini yang dapat dijangkau OnlyOffice, mis. http://localhost:8080
}

// PDFEnabled menandakan fitur konversi PDF dapat dipakai.
// Jika secret JWT kosong, fitur dimatikan namun jalur DOCX tetap berfungsi.
func (c Config) PDFEnabled() bool {
	return c.JWTSecret != "" && c.OnlyOfficeURL != "" && c.AppInternalURL != ""
}

// loadDotEnv membaca file .env sederhana (format KEY=VALUE) dan menanamkan
// nilainya ke environment proses HANYA jika key tersebut belum diset di OS.
// Dengan begitu environment OS selalu menang (memudahkan override saat deploy).
// Baris kosong dan baris diawali '#' diabaikan. File yang tidak ada bukan error.
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, val, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)

		// Buang tanda kutip pembungkus bila ada.
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}

		if key == "" {
			continue
		}
		if _, ada := os.LookupEnv(key); !ada {
			_ = os.Setenv(key, val)
		}
	}
	return scanner.Err()
}

// loadConfig memuat .env lalu menyusun Config dari environment.
// Memberi nilai default yang masuk akal untuk setup native satu host.
func loadConfig() (Config, error) {
	if err := loadDotEnv(".env"); err != nil {
		return Config{}, fmt.Errorf("gagal membaca .env: %w", err)
	}

	cfg := Config{
		OnlyOfficeURL:  envOr("ONLYOFFICE_URL", "http://localhost:8026"),
		JWTSecret:      os.Getenv("ONLYOFFICE_JWT_SECRET"),
		JWTHeader:      envOr("ONLYOFFICE_JWT_HEADER", "Authorization"),
		AppInternalURL: envOr("APP_INTERNAL_URL", "http://localhost:8080"),
	}

	// Rapikan trailing slash agar penyusunan URL konsisten.
	cfg.OnlyOfficeURL = strings.TrimRight(cfg.OnlyOfficeURL, "/")
	cfg.AppInternalURL = strings.TrimRight(cfg.AppInternalURL, "/")

	return cfg, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
