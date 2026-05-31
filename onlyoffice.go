package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// ====================== Token store DOCX in-memory ======================

const (
	docxTokenTTL    = 2 * time.Minute // umur token sebelum kedaluwarsa
	docxGCInterval  = 30 * time.Second
	maxConvertBytes = 100 << 20 // batas aman ukuran PDF yang diunduh (100 MB)
)

type docxEntry struct {
	data    []byte
	expires time.Time
}

// docxStore menyimpan byte DOCX sementara agar bisa diunduh OnlyOffice
// lewat endpoint internal. Token bersifat acak, berumur pendek, dan single-use.
type docxStore struct {
	mu    sync.Mutex
	items map[string]docxEntry
}

func newDocxStore() *docxStore {
	return &docxStore{items: make(map[string]docxEntry)}
}

// put menyimpan data dan mengembalikan token acak (hex 32 byte).
func (s *docxStore) put(data []byte) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("gagal membuat token acak: %w", err)
	}
	token := hex.EncodeToString(raw)

	s.mu.Lock()
	s.items[token] = docxEntry{data: data, expires: time.Now().Add(docxTokenTTL)}
	s.mu.Unlock()
	return token, nil
}

// getOnce mengambil data sekali pakai: entri langsung dihapus saat diambil.
// Mengembalikan (nil, false) bila token tidak ada atau sudah kedaluwarsa.
func (s *docxStore) getOnce(token string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.items[token]
	if !ok {
		return nil, false
	}
	delete(s.items, token)
	if time.Now().After(entry.expires) {
		return nil, false
	}
	return entry.data, true
}

// drop menghapus token tanpa membaca (dipakai untuk membersihkan setelah konversi).
func (s *docxStore) drop(token string) {
	s.mu.Lock()
	delete(s.items, token)
	s.mu.Unlock()
}

// runGC menjalankan pembersihan periodik entri kedaluwarsa. Memblokir, jalankan di goroutine.
func (s *docxStore) runGC() {
	ticker := time.NewTicker(docxGCInterval)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		s.mu.Lock()
		for token, entry := range s.items {
			if now.After(entry.expires) {
				delete(s.items, token)
			}
		}
		s.mu.Unlock()
	}
}

// ====================== JWT (HS256) ======================

// signJWT menghasilkan JWT HS256 atas payload menggunakan secret.
// Algoritma ini sudah diverifikasi diterima oleh Document Server target.
func signJWT(payload any, secret string) (string, error) {
	header := map[string]string{"alg": "HS256", "typ": "JWT"}

	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	enc := base64.RawURLEncoding
	signingInput := enc.EncodeToString(headerJSON) + "." + enc.EncodeToString(payloadJSON)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signingInput))
	signature := enc.EncodeToString(mac.Sum(nil))

	return signingInput + "." + signature, nil
}

// ====================== Klien konversi OnlyOffice ======================

type convertRequest struct {
	Async      bool   `json:"async"`
	Filetype   string `json:"filetype"`
	Outputtype string `json:"outputtype"`
	Key        string `json:"key"`
	Title      string `json:"title"`
	URL        string `json:"url"`
	Token      string `json:"token,omitempty"`
}

type convertResponse struct {
	FileURL    string `json:"fileUrl"`
	FileType   string `json:"fileType"`
	Percent    int    `json:"percent"`
	EndConvert bool   `json:"endConvert"`
	Error      int    `json:"error"`
}

// pesanErrorOnlyOffice memetakan kode error ConvertService ke pesan bahasa Indonesia.
// Referensi kode: dokumentasi OnlyOffice Conversion API.
func pesanErrorOnlyOffice(code int) string {
	switch code {
	case -1:
		return "Kesalahan tak terduga pada server konversi"
	case -2:
		return "Waktu konversi habis (dokumen mungkin terlalu besar)"
	case -3:
		return "Server konversi gagal memproses dokumen"
	case -4:
		return "Server konversi gagal mengunduh dokumen sumber"
	case -5:
		return "Kesalahan enkripsi/dekripsi saat konversi"
	case -6:
		return "Kesalahan saat mengakses basis data konversi"
	case -8:
		return "Token JWT tidak valid (periksa secret OnlyOffice)"
	case -9:
		return "Format konversi tidak didukung"
	default:
		return fmt.Sprintf("Kode error konversi tidak dikenal (%d)", code)
	}
}

// convertDocxToPDF meminta OnlyOffice mengonversi DOCX (yang diekspos pada
// docxURL) menjadi PDF secara sinkron, lalu mengunduh dan mengembalikan byte PDF.
// docxData dipakai untuk menghitung key konversi yang stabil per konten.
func convertDocxToPDF(cfg Config, docxURL, title string, docxData []byte) ([]byte, error) {
	if !cfg.PDFEnabled() {
		return nil, fmt.Errorf("fitur PDF tidak aktif: konfigurasi OnlyOffice belum lengkap")
	}

	sum := sha256.Sum256(docxData)
	key := hex.EncodeToString(sum[:])[:20] // key <= 128 char; 20 sudah cukup unik & stabil

	reqBody := convertRequest{
		Async:      false,
		Filetype:   "docx",
		Outputtype: "pdf",
		Key:        key,
		Title:      title,
		URL:        docxURL,
	}

	// Tanda tangani body sebagai payload JWT (mode inbox memverifikasi ini).
	token, err := signJWT(reqBody, cfg.JWTSecret)
	if err != nil {
		return nil, fmt.Errorf("gagal menandatangani permintaan konversi: %w", err)
	}
	reqBody.Token = token

	bodyJSON, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	client := &http.Client{Timeout: 120 * time.Second}

	httpReq, err := http.NewRequest(http.MethodPost, cfg.OnlyOfficeURL+"/ConvertService.ashx", bytes.NewReader(bodyJSON))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set(cfg.JWTHeader, "Bearer "+token)

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("tidak dapat menghubungi server OnlyOffice: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("gagal membaca respons OnlyOffice: %w", err)
	}

	var cr convertResponse
	if err := json.Unmarshal(respBytes, &cr); err != nil {
		return nil, fmt.Errorf("respons OnlyOffice tidak dapat diurai (HTTP %d): %s", resp.StatusCode, string(respBytes))
	}

	if cr.Error != 0 {
		return nil, fmt.Errorf("%s", pesanErrorOnlyOffice(cr.Error))
	}
	if !cr.EndConvert || cr.FileURL == "" {
		return nil, fmt.Errorf("konversi belum selesai (percent=%d)", cr.Percent)
	}

	// Unduh hasil PDF dari fileUrl yang diberikan OnlyOffice.
	pdfBytes, err := unduhFile(client, cr.FileURL)
	if err != nil {
		return nil, fmt.Errorf("gagal mengunduh PDF hasil konversi: %w", err)
	}
	return pdfBytes, nil
}

func unduhFile(client *http.Client, url string) ([]byte, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d saat mengunduh", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxConvertBytes))
}
