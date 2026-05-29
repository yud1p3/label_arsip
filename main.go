package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
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

// Batasi maksimal 4 proses konversi biner berat secara bersamaan (proteksi CPU 2 Core)
var semaphore = make(chan struct{}, 4)

func main() {
	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()

	// Daftarkan folder templates agar bisa membaca template DOCX sekaligus HTML
	r.LoadHTMLGlob("templates/*")

	r.MaxMultipartMemory = 5 << 20
	r.POST("/api/generate-label", generateLabelWebHandler)
	r.GET("/", func(c *gin.Context) {
		c.HTML(http.StatusOK, "index.html", nil)
	})

	fmt.Println("Aplikasi Cetak Label berjalan di http://localhost:8080")
	r.Run(":8080")
}

func generateLabelWebHandler(c *gin.Context) {
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

	var daftarArsip []LabelData
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
	semaphore <- struct{}{}
	defer func() { <-semaphore }()

	// Jalur template relatif terhadap root folder project web
	pathTemplate := "templates/template_label_map_arsip.docx"

	// 4. Olah Dokumen secara In-Memory
	fileFinalBytes, err := prosesMultiHalamanInMemory(pathTemplate, daftarArsip)
	if err != nil {
		log.Printf("[Error] Gagal memproses dokumen: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Terjadi kegagalan internal saat menyusun dokumen Word"})
		return
	}

	// 5. Transfer langsung byte array .docx ke browser (Direct Download Stream)
	namaFileDownload := fmt.Sprintf("Buku_Label_Arsip_%d.docx", time.Now().Unix())
	c.Header("Content-Description", "File Transfer")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%s", namaFileDownload))
	c.Header("Content-Type", "application/vnd.openxmlformats-officedocument.wordprocessingml.document")
	c.Header("Content-Length", strconv.Itoa(len(fileFinalBytes)))

	_, _ = c.Writer.Write(fileFinalBytes)
}

func prosesMultiHalamanInMemory(templatePath string, daftarArsip []LabelData) ([]byte, error) {
	totalData := len(daftarArsip)
	var semuaBarisBaru []byte
	reBaris := regexp.MustCompile(`<w:tr\b[^>]*?>.*?</w:tr>`)

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
		xmlKonten, err := buatHalamanXmlInMemory(templatePath, chunkData, i)
		if err != nil {
			return nil, err
		}

		if halamanKe == 1 {
			xmlUtama = xmlKonten
		} else {
			barisBaris := reBaris.FindAll(xmlKonten, -1)
			for _, baris := range barisBaris {
				semuaBarisBaru = append(semuaBarisBaru, baris...)
			}
		}
	}

	// Cari tag penutup tabel </w:tbl>
	reTabelPenutup := regexp.MustCompile(`</w:tbl>`)
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
	templateReader, err := zip.OpenReader(templatePath)
	if err != nil {
		return nil, err
	}
	defer templateReader.Close()

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

func buatHalamanXmlInMemory(templatePath string, chunkData []LabelData, startIndex int) ([]byte, error) {
	reader, err := zip.OpenReader(templatePath)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	var xmlBytes []byte
	for _, file := range reader.File {
		if file.Name == "word/document.xml" {
			fileReader, err := file.Open()
			if err != nil {
				return nil, err
			}
			buf := new(bytes.Buffer)
			_, _ = io.Copy(buf, fileReader)
			fileReader.Close()
			xmlBytes = buf.Bytes()
			break
		}
	}

	// Bersihkan split-run XML Word
	re := regexp.MustCompile(`%[^%]*?<[^>]*?>[^%]*?%`)
	xmlBytes = re.ReplaceAllFunc(xmlBytes, func(s []byte) []byte {
		cleanRegex := regexp.MustCompile(`<[^>]*?>`)
		return cleanRegex.ReplaceAll(s, []byte(""))
	})

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

	return xmlBytes, nil
}
