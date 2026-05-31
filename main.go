package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/xuri/excelize/v2"
)

type LabelData struct {
	No    string
	Klas  string
	Judul string
	Tahun string
}

const labelPerHalaman = 20

var (
	reBaris        = regexp.MustCompile(`<w:tr\b[^>]*?>.*?</w:tr>`)
	reTabelPenutup = regexp.MustCompile(`</w:tbl>`)
	reSplitRunXML  = regexp.MustCompile(`%[^%]*?<[^>]*?>[^%]*?%`)
	reXMLTag       = regexp.MustCompile(`<[^>]*?>`)
)

// app menampung dependensi runtime (konfigurasi & penyimpanan DOCX sementara)
// yang dibutuhkan handler. Menghindari variabel global yang tersebar.
type app struct {
	cfg          Config
	store        *docxStore
	templateDOCX []byte
	templateXML  []byte
	jobs         chan struct{}
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("Gagal memuat konfigurasi: %v", err)
	}

	templatePath := "templates/template_label_map_arsip.docx"
	templateDOCX, err := os.ReadFile(templatePath)
	if err != nil {
		log.Fatalf("Gagal membaca template DOCX: %v", err)
	}

	templateXML, err := extractTemplateDocumentXML(templateDOCX)
	if err != nil {
		log.Fatalf("Gagal mengekstrak XML template DOCX: %v", err)
	}

	a := &app{
		cfg:          cfg,
		store:        newDocxStore(),
		templateDOCX: templateDOCX,
		templateXML:  templateXML,
		jobs:         make(chan struct{}, cfg.MaxConcurrentJobs),
	}
	go a.store.runGC()

	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()

	// Daftarkan folder templates agar bisa membaca template DOCX sekaligus HTML
	r.LoadHTMLGlob("templates/*")

	r.MaxMultipartMemory = 5 << 20
	r.POST("/api/generate-label", a.generateLabelWebHandler)
	// Endpoint internal: hanya untuk diakses OnlyOffice (loopback) mengambil DOCX sementara.
	r.GET("/internal/docx/:token", a.serveInternalDocx)
	r.GET("/", func(c *gin.Context) {
		c.HTML(http.StatusOK, "index.html", nil)
	})

	fmt.Println("Batas pekerjaan berat paralel:", cfg.MaxConcurrentJobs)
	if cfg.PDFEnabled() {
		fmt.Println("Fitur PDF AKTIF (OnlyOffice:", cfg.OnlyOfficeURL+")")
	} else {
		fmt.Println("Fitur PDF NONAKTIF (secret/URL OnlyOffice belum lengkap) — hanya DOCX yang tersedia")
	}
	fmt.Println("Aplikasi Cetak Label berjalan di http://localhost:8080")
	r.Run(":8080")
}

// serveInternalDocx menyajikan byte DOCX sementara berdasarkan token sekali pakai.
// Akses dibatasi hanya dari loopback (OnlyOffice native berjalan di host yang sama),
// sehingga host lain di LAN tidak dapat mengambil dokumen meski menebak token.
func (a *app) serveInternalDocx(c *gin.Context) {
	ip := net.ParseIP(c.ClientIP())
	if ip == nil || !ip.IsLoopback() {
		c.JSON(http.StatusForbidden, gin.H{"error": "Akses ditolak"})
		return
	}

	token := c.Param("token")
	data, ok := a.store.getOnce(token)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "Dokumen tidak ditemukan atau sudah kedaluwarsa"})
		return
	}

	c.Header("Content-Type", "application/vnd.openxmlformats-officedocument.wordprocessingml.document")
	c.Header("Content-Length", strconv.Itoa(len(data)))
	_, _ = c.Writer.Write(data)
}

func (a *app) generateLabelWebHandler(c *gin.Context) {
	// Format keluaran: "docx" (default) atau "pdf".
	format := c.PostForm("format")
	if format == "" {
		format = "docx"
	}
	if format == "pdf" && !a.cfg.PDFEnabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Fitur konversi PDF sedang tidak tersedia di server"})
		return
	}

	// 1. Tangkap file Excel dari Form Upload Multipart
	fileHeader, err := c.FormFile("excel_file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Gagal menerima file excel_file"})
		return
	}

	file, err := fileHeader.Open()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal membuka stream file"})
		return
	}
	defer file.Close()

	// 2. Baca Excel langsung dari memori
	excelFile, err := excelize.OpenReader(file)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Format file Excel korup atau tidak valid"})
		return
	}
	defer excelFile.Close()

	rows, err := excelFile.GetRows("Sheet1")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Gagal membaca data dari Sheet1"})
		return
	}

	kapasitasDaftar := 0
	if len(rows) > 1 {
		kapasitasDaftar = len(rows) - 1
	}
	daftarArsip := make([]LabelData, 0, kapasitasDaftar)
	for i, row := range rows {
		// Lewati header Excel (baris pertama) atau baris kosong yang tidak lengkap
		if i == 0 || len(row) < 4 {
			continue
		}
		daftarArsip = append(daftarArsip, LabelData{
			No:    row[0],
			Klas:  row[1],
			Judul: row[2],
			Tahun: row[3],
		})
	}

	if len(daftarArsip) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Tidak ada baris data arsip yang valid untuk diproses"})
		return
	}

	// 3. Amankan CPU Resource dengan Antrean Semaphore
	a.jobs <- struct{}{}
	defer func() { <-a.jobs }()

	// 4. Olah Dokumen secara In-Memory
	fileFinalBytes, err := prosesMultiHalamanInMemory(a.templateDOCX, a.templateXML, daftarArsip)
	if err != nil {
		log.Printf("[Error] Gagal memproses dokumen: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Terjadi kegagalan internal saat menyusun dokumen Word"})
		return
	}

	// 5. Kirim hasil sesuai format yang diminta.
	if format == "pdf" {
		a.kirimSebagaiPDF(c, fileFinalBytes)
		return
	}

	// Default: transfer langsung byte array .docx ke browser (Direct Download Stream)
	namaFileDownload := namaFileUnduhan("docx")
	c.Header("Content-Description", "File Transfer")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%s", namaFileDownload))
	c.Header("Content-Type", "application/vnd.openxmlformats-officedocument.wordprocessingml.document")
	c.Header("Content-Length", strconv.Itoa(len(fileFinalBytes)))

	_, _ = c.Writer.Write(fileFinalBytes)
}

// kirimSebagaiPDF menyimpan DOCX ke store sementara, memintanya dikonversi oleh
// OnlyOffice, lalu men-stream PDF hasilnya ke browser. Token DOCX dipastikan
// terhapus walau konversi gagal.
func (a *app) kirimSebagaiPDF(c *gin.Context, docxBytes []byte) {
	token, err := a.store.put(docxBytes)
	if err != nil {
		log.Printf("[Error] Gagal menyiapkan token DOCX: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal menyiapkan dokumen untuk konversi"})
		return
	}
	// Jaring pengaman: jika getOnce tidak terpanggil (mis. konversi gagal sebelum
	// OnlyOffice mengambil), token tetap dibersihkan.
	defer a.store.drop(token)

	docxURL := a.cfg.AppInternalURL + "/internal/docx/" + token
	pdfBytes, err := convertDocxToPDF(a.cfg, docxURL, "label_arsip.docx", docxBytes)
	if err != nil {
		log.Printf("[Error] Konversi PDF gagal: %v", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "Gagal mengonversi ke PDF: " + err.Error()})
		return
	}

	namaFileDownload := namaFileUnduhan("pdf")
	c.Header("Content-Description", "File Transfer")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%s", namaFileDownload))
	c.Header("Content-Type", "application/pdf")
	c.Header("Content-Length", strconv.Itoa(len(pdfBytes)))

	_, _ = c.Writer.Write(pdfBytes)
}

func prosesMultiHalamanInMemory(templateDOCX, templateXML []byte, daftarArsip []LabelData) ([]byte, error) {
	totalData := len(daftarArsip)
	var semuaBarisBaru []byte

	var xmlUtama []byte
	halamanKe := 0

	for i := 0; i < totalData; i += labelPerHalaman {
		halamanKe++
		end := i + labelPerHalaman
		if end > totalData {
			end = totalData
		}
		chunkData := daftarArsip[i:end]

		// Ekstrak XML halaman dari memori
		xmlKonten := buatHalamanXmlInMemory(templateXML, chunkData, i)

		if halamanKe == 1 {
			xmlUtama = xmlKonten
		} else {
			barisBaris := reBaris.FindAll(xmlKonten, -1)
			for _, baris := range barisBaris {
				semuaBarisBaru = append(semuaBarisBaru, baris...)
			}
		}
	}

	locs := reTabelPenutup.FindAllIndex(xmlUtama, -1)
	if len(locs) == 0 {
		return nil, fmt.Errorf("tag </w:tbl> tidak ditemukan di template")
	}
	posisiSisip := locs[len(locs)-1][0] // Sisipkan tepat SEBELUM </w:tbl> halaman pertama

	xmlFinal := make([]byte, 0, len(xmlUtama)+len(semuaBarisBaru))
	xmlFinal = append(xmlFinal, xmlUtama[:posisiSisip]...)
	xmlFinal = append(xmlFinal, semuaBarisBaru...)
	xmlFinal = append(xmlFinal, xmlUtama[posisiSisip:]...)

	// Rekonstruksi struktur ZIP .docx utuh langsung ke memory buffer
	templateReader, err := zip.NewReader(bytes.NewReader(templateDOCX), int64(len(templateDOCX)))
	if err != nil {
		return nil, err
	}

	outputBuf := new(bytes.Buffer)
	zipWriter := zip.NewWriter(outputBuf)

	for _, file := range templateReader.File {
		fileWriter, err := zipWriter.Create(file.Name)
		if err != nil {
			return nil, err
		}

		if file.Name == "word/document.xml" {
			_, err = fileWriter.Write(xmlFinal)
		} else {
			fileReader, err := file.Open()
			if err != nil {
				return nil, err
			}
			_, err = io.Copy(fileWriter, fileReader)
			fileReader.Close()
		}
		if err != nil {
			return nil, err
		}
	}
	_ = zipWriter.Close()

	return outputBuf.Bytes(), nil
}

func extractTemplateDocumentXML(templateDOCX []byte) ([]byte, error) {
	reader, err := zip.NewReader(bytes.NewReader(templateDOCX), int64(len(templateDOCX)))
	if err != nil {
		return nil, err
	}

	for _, file := range reader.File {
		if file.Name != "word/document.xml" {
			continue
		}

		fileReader, err := file.Open()
		if err != nil {
			return nil, err
		}

		buf := new(bytes.Buffer)
		_, err = io.Copy(buf, fileReader)
		fileReader.Close()
		if err != nil {
			return nil, err
		}

		xmlBytes := buf.Bytes()
		xmlBytes = reSplitRunXML.ReplaceAllFunc(xmlBytes, func(s []byte) []byte {
			return reXMLTag.ReplaceAll(s, []byte(""))
		})
		return xmlBytes, nil
	}

	return nil, fmt.Errorf("file word/document.xml tidak ditemukan di template DOCX")
}

func namaFileUnduhan(ext string) string {
	sekarang := time.Now()
	return fmt.Sprintf("Buku_Label_Arsip_%d_%09d.%s", sekarang.Unix(), sekarang.Nanosecond(), ext)
}

func buatHalamanXmlInMemory(templateXML []byte, chunkData []LabelData, startIndex int) []byte {
	xmlBytes := append([]byte(nil), templateXML...)

	// Inject data riil
	for index, data := range chunkData {
		suffix := strconv.Itoa(index + 1)
		nomorAbsolut := strconv.Itoa(startIndex + index + 1)

		xmlBytes = bytes.ReplaceAll(xmlBytes, []byte("%no"+suffix+"%"), []byte(nomorAbsolut))
		xmlBytes = bytes.ReplaceAll(xmlBytes, []byte("%klas"+suffix+"%"), []byte(data.Klas))
		xmlBytes = bytes.ReplaceAll(xmlBytes, []byte("%judul"+suffix+"%"), []byte(data.Judul))
		xmlBytes = bytes.ReplaceAll(xmlBytes, []byte("%tahun"+suffix+"%"), []byte(data.Tahun))
	}

	// Kosongkan sisa slot jika data di halaman terakhir kurang dari 20
	if len(chunkData) < labelPerHalaman {
		for k := len(chunkData) + 1; k <= labelPerHalaman; k++ {
			suffix := strconv.Itoa(k)
			xmlBytes = bytes.ReplaceAll(xmlBytes, []byte("%no"+suffix+"%"), []byte(""))
			xmlBytes = bytes.ReplaceAll(xmlBytes, []byte("%klas"+suffix+"%"), []byte(""))
			xmlBytes = bytes.ReplaceAll(xmlBytes, []byte("%judul"+suffix+"%"), []byte(""))
			xmlBytes = bytes.ReplaceAll(xmlBytes, []byte("%tahun"+suffix+"%"), []byte(""))
		}
	}

	return xmlBytes
}
