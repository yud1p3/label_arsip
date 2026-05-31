# Checklist Deploy VPS Ubuntu

Checklist ini dibuat untuk deployment `label_arsip` di VPS Ubuntu dengan spesifikasi sekitar **2 core / 4 GB RAM**.

## 1. Persiapan server

- [ ] Login ke VPS dengan user yang punya akses `sudo`
- [ ] Jalankan update sistem:
  ```sh
  sudo apt update && sudo apt upgrade -y
  ```
- [ ] Install paket dasar:
  ```sh
  sudo apt install -y git curl nginx
  ```
- [ ] Install Go jika ingin build langsung di VPS
- [ ] Install Docker jika ingin menjalankan OnlyOffice di server yang sama

## 2. Ambil source code

- [ ] Clone repo:
  ```sh
  git clone https://github.com/yud1p3/label_arsip.git
  cd label_arsip
  ```
- [ ] Pastikan branch yang dipakai adalah `main`
- [ ] Jalankan:
  ```sh
  git pull origin main
  ```

## 3. Siapkan file environment

- [ ] Salin contoh environment:
  ```sh
  cp .env.example .env
  ```
- [ ] Isi nilai pada `.env`
- [ ] Untuk VPS `2 core / 4 GB`, gunakan:
  ```env
  MAX_CONCURRENT_JOBS=2
  ```
- [ ] Jika PDF diaktifkan, isi juga:
  - [ ] `ONLYOFFICE_URL`
  - [ ] `ONLYOFFICE_JWT_SECRET`
  - [ ] `ONLYOFFICE_JWT_HEADER`
  - [ ] `APP_INTERNAL_URL`

Contoh aman untuk setup satu VPS:

```env
ONLYOFFICE_URL=http://127.0.0.1:8026
ONLYOFFICE_JWT_SECRET=ganti-dengan-secret-anda
ONLYOFFICE_JWT_HEADER=Authorization
APP_INTERNAL_URL=http://127.0.0.1:8080
MAX_CONCURRENT_JOBS=2
```

## 4. Build aplikasi

- [ ] Masuk ke folder project
- [ ] Build binary:
  ```sh
  go build -o label-arsip-prod .
  ```
- [ ] Pastikan file `label-arsip-prod` berhasil dibuat
- [ ] Pastikan folder `templates/` ikut tersedia di server

## 5. Uji jalan aplikasi secara manual

- [ ] Jalankan aplikasi:
  ```sh
  ./label-arsip-prod
  ```
- [ ] Buka di browser:
  ```text
  http://IP-VPS:8080
  ```
- [ ] Uji upload Excel sample
- [ ] Uji generate DOCX
- [ ] Jika PDF diaktifkan, uji generate PDF
- [ ] Hentikan aplikasi manual setelah pengujian awal selesai

## 6. Pasang service systemd

- [ ] Buat file service:
  ```sh
  sudo nano /etc/systemd/system/label-arsip.service
  ```
- [ ] Isi service sesuai path project di VPS
- [ ] Pastikan `EnvironmentFile` mengarah ke file `.env`
- [ ] Reload systemd:
  ```sh
  sudo systemctl daemon-reload
  ```
- [ ] Enable service:
  ```sh
  sudo systemctl enable label-arsip
  ```
- [ ] Start service:
  ```sh
  sudo systemctl start label-arsip
  ```
- [ ] Cek status:
  ```sh
  sudo systemctl status label-arsip
  ```

## 7. Pasang reverse proxy Nginx

- [ ] Buat config Nginx untuk aplikasi
- [ ] Set `proxy_pass` ke:
  ```text
  http://127.0.0.1:8080
  ```
- [ ] Set `client_max_body_size` minimal `10M`
- [ ] Aktifkan site Nginx
- [ ] Uji konfigurasi:
  ```sh
  sudo nginx -t
  ```
- [ ] Reload Nginx:
  ```sh
  sudo systemctl reload nginx
  ```

## 8. Setup OnlyOffice untuk PDF

Lewati bagian ini jika hanya memakai output DOCX.

- [ ] Jalankan OnlyOffice di VPS yang sama
- [ ] Bind OnlyOffice hanya ke loopback:
  ```text
  127.0.0.1:8026
  ```
- [ ] Samakan secret Docker dengan `ONLYOFFICE_JWT_SECRET`
- [ ] Pastikan aplikasi bisa mengakses:
  ```text
  http://127.0.0.1:8026
  ```
- [ ] Pastikan `APP_INTERNAL_URL=http://127.0.0.1:8080`
- [ ] Uji generate PDF dari browser

## 9. Buka akses jaringan seperlunya

- [ ] Pastikan port `80` terbuka untuk HTTP
- [ ] Jika pakai HTTPS nanti, buka port `443`
- [ ] Jangan buka port OnlyOffice ke publik jika tidak perlu
- [ ] Jangan expose file `.env`

## 10. Verifikasi akhir

- [ ] `systemd` status aplikasi = aktif
- [ ] `nginx` status = aktif
- [ ] Halaman utama bisa dibuka dari browser
- [ ] Upload Excel berhasil
- [ ] Generate DOCX berhasil
- [ ] Generate PDF berhasil (jika diaktifkan)
- [ ] Tidak ada error penting di log aplikasi
- [ ] Tidak ada error penting di log Nginx

## 11. Monitoring setelah deploy

- [ ] Pantau log aplikasi:
  ```sh
  sudo journalctl -u label-arsip -f
  ```
- [ ] Pantau log Nginx:
  ```sh
  sudo tail -f /var/log/nginx/access.log /var/log/nginx/error.log
  ```
- [ ] Jika server terasa berat, pastikan `MAX_CONCURRENT_JOBS=2`
- [ ] Naikkan ke `3` hanya jika CPU dan RAM masih aman

## 12. Checklist update aplikasi berikutnya

- [ ] `git pull origin main`
- [ ] Build ulang:
  ```sh
  go build -o label-arsip-prod .
  ```
- [ ] Restart service:
  ```sh
  sudo systemctl restart label-arsip
  ```
- [ ] Uji ulang DOCX dan PDF
