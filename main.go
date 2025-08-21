package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
)

// ------------------- EMBED FRONTEND -------------------

//go:embed web/*
var content embed.FS

func indexHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		data, err := content.ReadFile("web/index.html")
		if err != nil {
			log.Printf("erro ao ler index.html: %v", err)
			http.Error(w, "Erro ao carregar página", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if _, err := w.Write(data); err != nil {
			log.Printf("erro ao escrever resposta: %v", err)
			return
		}
		return
	}
	http.FileServer(http.FS(content)).ServeHTTP(w, r)
}

// ------------------- STATUS (progresso simples) -------------------

type appStatus struct {
	mu      sync.Mutex
	Running bool   `json:"running"`
	Stage   string `json:"stage"` // "Aguardando", "Baixando", "Convertendo", "Finalizado", "Erro", "Cancelado"
	Detail  string `json:"detail"`
}

var statusState = &appStatus{Stage: "Aguardando", Detail: ""}

func setStage(stage, detail string, running bool) {
	statusState.mu.Lock()
	statusState.Stage = stage
	statusState.Detail = detail
	statusState.Running = running
	statusState.mu.Unlock()
}

func getStatus() appStatus {
	statusState.mu.Lock()
	defer statusState.mu.Unlock()
	return appStatus{
		Running: statusState.Running,
		Stage:   statusState.Stage,
		Detail:  statusState.Detail,
	}
}

// ------------------- CANCELAMENTO GLOBAL -------------------

var cancelMu sync.Mutex
var currentCancel context.CancelFunc

func setCurrentCancel(cf context.CancelFunc) {
	cancelMu.Lock()
	currentCancel = cf
	cancelMu.Unlock()
}
func clearCurrentCancel() {
	cancelMu.Lock()
	currentCancel = nil
	cancelMu.Unlock()
}

func cancelHandler(w http.ResponseWriter, r *http.Request) {
	cancelMu.Lock()
	cf := currentCancel
	cancelMu.Unlock()

	if cf == nil {
		http.Error(w, "Nenhuma tarefa em execução.", http.StatusBadRequest)
		return
	}
	cf() // interrompe yt-dlp/ffmpeg
	clearCurrentCancel()
	setStage("Cancelado", "Ação cancelada pelo usuário.", false)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// ------------------- REQ / UTIL -------------------

type actionReq struct {
	Action    string `json:"action"` // "download" | "convert"
	URL       string `json:"url"`
	Profile   string `json:"profile"`   // "original" | "whatsapp" | "480p" | "720p" | "1080p" | "4k" | "mp3"
	Start     string `json:"start"`     // "HH:MM:SS" (opcional)
	End       string `json:"end"`       // "HH:MM:SS" (opcional)
	OutputDir string `json:"outputDir"` // diretório escolhido (OBRIGATÓRIO)
}

func exeDir() string {
	p, _ := os.Executable()
	return filepath.Dir(p)
}

func cwdDir() string {
	d, _ := os.Getwd()
	return d
}

// procura executáveis em exeDir, CWD e PATH
func findTool(name string) (string, error) {
	p1 := filepath.Join(exeDir(), name)
	if _, err := os.Stat(p1); err == nil {
		return p1, nil
	}
	p2 := filepath.Join(cwdDir(), name)
	if _, err := os.Stat(p2); err == nil {
		return p2, nil
	}
	if p3, err := exec.LookPath(name); err == nil {
		return p3, nil
	}
	return "", fmt.Errorf("%s não encontrado (procurei em exeDir, CWD e PATH)", name)
}

func ensureDirs() (string, string, error) {
	base := exeDir()
	temp := filepath.Join(base, "temp")
	out := filepath.Join(base, "output")
	if err := os.MkdirAll(temp, 0755); err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(out, 0755); err != nil {
		return "", "", err
	}
	return temp, out, nil
}

func timestampName() string {
	return time.Now().Format("20060102-150405")
}

func openBrowser(url string) {
	switch runtime.GOOS {
	case "windows":
		_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "linux":
		_ = exec.Command("xdg-open", url).Start()
	case "darwin":
		_ = exec.Command("open", url).Start()
	}
}

func openInExplorerSelect(path string) {
	if runtime.GOOS == "windows" {
		_ = exec.Command("explorer", "/select,", path).Start()
	}
}

func findFirstGlob(pattern string) (string, error) {
	matches, _ := filepath.Glob(pattern)
	if len(matches) == 0 {
		return "", errors.New("arquivo não encontrado pelo padrão: " + pattern)
	}
	return matches[0], nil
}

func validateTimes(start, end string) error {
	if start == "" && end == "" {
		return nil
	}
	if start == "" || end == "" {
		return errors.New("preencha início e fim juntos ou deixe ambos em branco")
	}

	re := regexp.MustCompile(`^[0-9]{2}:[0-5][0-9]:[0-5][0-9]$`)
	if !re.MatchString(start) || !re.MatchString(end) {
		return errors.New("tempos devem estar no formato HH:MM:SS")
	}

	toSeconds := func(t string) int {
		parts := strings.Split(t, ":")
		h, _ := strconv.Atoi(parts[0])
		m, _ := strconv.Atoi(parts[1])
		s, _ := strconv.Atoi(parts[2])
		return h*3600 + m*60 + s
	}

	if toSeconds(start) >= toSeconds(end) {
		return errors.New("tempo inicial deve ser menor que o tempo final")
	}

	return nil
}

func runCmdWithLog(cmd *exec.Cmd) (string, error) {
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	combined := strings.TrimSpace(out.String() + "\n" + errb.String())
	if err != nil {
		if combined == "" {
			return "", err
		}
		return combined, err
	}
	return combined, nil
}

// ------------------- LIMPEZA (CANCELAMENTO/ERROS) -------------------

func ctxCanceled(ctx context.Context) bool {
	return errors.Is(ctx.Err(), context.Canceled)
}

func removeIfExists(path string) {
	if path == "" {
		return
	}
	_ = os.Remove(path)
}

func removeGlob(pattern string) {
	files, _ := filepath.Glob(pattern)
	for _, f := range files {
		_ = os.RemoveAll(f)
	}
}

func cleanupConvertTemp(tempDir, tempPattern, outFile string) {
	// remove arquivo temporário baixado
	removeGlob(tempPattern)
	// remove diretório temporário (caso esteja vazio ou não)
	if tempDir != "" {
		_ = os.RemoveAll(tempDir)
	}
	// remove saída parcial se existir
	removeIfExists(outFile)
}

func cleanupDownloadArtifacts(outPrefix string) {
	// remove quaisquer artefatos baixados/mesclados com o prefixo (ex.: .mp4.part, .webm, .m4a etc.)
	if outPrefix == "" {
		return
	}
	removeGlob(outPrefix + ".*")
}

// ------------------- TÍTULO E NOMES -------------------

var invalidNameChars = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1F]`)

func sanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	name = invalidNameChars.ReplaceAllString(name, "_")
	name = regexp.MustCompile(`\s+`).ReplaceAllString(name, " ")
	name = strings.TrimSpace(name)
	if len(name) == 0 {
		name = "video"
	}
	if len(name) > 150 {
		name = name[:150]
	}
	return name
}

func getVideoTitle(ctx context.Context, url string) (string, error) {
	yt, err := findTool("yt-dlp.exe")
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, yt, "--get-title", "--no-playlist", url)
	out, err := runCmdWithLog(cmd)
	if err != nil {
		return "", fmt.Errorf("falha ao obter título: %v\n%s", err, out)
	}
	title := strings.TrimSpace(out)
	if title == "" {
		return "", errors.New("título vazio")
	}
	return sanitizeFilename(title), nil
}

func outputExt(profile string) string {
	if strings.ToLower(profile) == "mp3" {
		return ".mp3"
	}
	return ".mp4"
}

func resolveOutputDir(custom string) (string, error) {
	if custom == "" {
		return "", errors.New("pasta de destino é obrigatória")
	}
	if err := os.MkdirAll(custom, 0755); err != nil {
		return "", err
	}
	return custom, nil
}

// ------------------- PRESETS / FFMPEG -------------------

func ffmpegArgsPreset(profile string) []string {
	switch strings.ToLower(profile) {
	case "original":
		return []string{"-c", "copy"}

	case "whatsapp":
		return []string{
			"-vf", "scale=1280:-1",
			"-c:v", "libx264", "-pix_fmt", "yuv420p",
			"-preset", "veryfast", "-crf", "28",
			"-c:a", "aac", "-b:a", "128k",
			"-movflags", "+faststart",
		}

	case "480p":
		return []string{
			"-vf", "scale=-2:480",
			"-c:v", "libx264", "-pix_fmt", "yuv420p",
			"-preset", "veryfast", "-crf", "27",
			"-c:a", "aac", "-b:a", "128k",
			"-movflags", "+faststart",
		}

	case "720p":
		return []string{
			"-vf", "scale=-2:720",
			"-c:v", "libx264", "-pix_fmt", "yuv420p",
			"-preset", "fast", "-crf", "23",
			"-c:a", "aac", "-b:a", "160k",
			"-movflags", "+faststart",
		}

	case "1080p":
		return []string{
			"-vf", "scale=-2:1080",
			"-c:v", "libx264", "-pix_fmt", "yuv420p",
			"-preset", "fast", "-crf", "20",
			"-c:a", "aac", "-b:a", "192k",
			"-movflags", "+faststart",
		}

	case "4k", "2160p", "uhd":
		return []string{
			"-vf", "scale=-2:2160",
			"-c:v", "libx264", "-pix_fmt", "yuv420p",
			"-preset", "slow", "-crf", "18",
			"-c:a", "aac", "-b:a", "192k",
			"-movflags", "+faststart",
		}

	case "mp3":
		return []string{
			"-vn",
			"-c:a", "libmp3lame",
			"-b:a", "160k",
		}
	}
	// fallback
	return []string{
		"-c:v", "libx264", "-pix_fmt", "yuv420p",
		"-preset", "veryfast", "-crf", "23",
		"-c:a", "aac", "-b:a", "160k",
	}
}

// ------------------- PIPELINES -------------------

// BAIXAR (sem recodificar) com nome baseado no título
func handleDownload(ctx context.Context, url, outDir string) (string, error) {
	setStage("Baixando", "Sem recodificação (apenas mesclando streams)", true)

	_, _, err := ensureDirs()
	if err != nil {
		setStage("Erro", err.Error(), false)
		return "", err
	}
	yt, err := findTool("yt-dlp.exe")
	if err != nil {
		setStage("Erro", err.Error(), false)
		return "", err
	}
	targetDir, err := resolveOutputDir(outDir)
	if err != nil {
		setStage("Erro", err.Error(), false)
		return "", err
	}

	title, err := getVideoTitle(ctx, url)
	if err != nil {
		title = "video-" + timestampName()
	}

	outPrefix := filepath.Join(targetDir, title)
	args := []string{
		"-o", outPrefix + ".%(ext)s",
		"-f", "bv*[ext=mp4]+ba[ext=m4a]/b[ext=mp4]/bv*+ba/b",
		"--merge-output-format", "mp4",
		url,
	}
	cmd := exec.CommandContext(ctx, yt, args...)
	log, runErr := runCmdWithLog(cmd)
	if runErr != nil {
		if ctxCanceled(ctx) {
			// limpa artefatos parciais do download
			cleanupDownloadArtifacts(outPrefix)
			return "", context.Canceled
		}
		setStage("Erro", fmt.Sprintf("yt-dlp falhou:\n%s", log), false)
		return "", fmt.Errorf("falha ao baixar (yt-dlp): %v\n%s", runErr, log)
	}

	outFile := filepath.Join(targetDir, title+".mp4")
	if _, err := os.Stat(outFile); err != nil {
		alt, err2 := findFirstGlob(outPrefix + ".*")
		if err2 != nil {
			setStage("Erro", "Não encontrei o arquivo de saída.", false)
			return "", fmt.Errorf("arquivo de saída não encontrado")
		}
		outFile = alt
	}
	openInExplorerSelect(outFile)
	setStage("Finalizado", "Download concluído", false)
	return outFile, nil
}

// CONVERTER (recodifica; corte opcional) – inclui MP3 – com nome pelo título
func handleConvert(ctx context.Context, url, profile, start, end, outDir string) (string, error) {
	if strings.ToLower(profile) == "original" {
		return handleDownload(ctx, url, outDir)
	}
	if err := validateTimes(start, end); err != nil {
		setStage("Erro", err.Error(), false)
		return "", err
	}
	tempDir, _, err := ensureDirs()
	if err != nil {
		setStage("Erro", err.Error(), false)
		return "", err
	}
	yt, err := findTool("yt-dlp.exe")
	if err != nil {
		setStage("Erro", err.Error(), false)
		return "", err
	}
	ff, err := findTool("ffmpeg.exe")
	if err != nil {
		setStage("Erro", err.Error(), false)
		return "", err
	}

	targetDir, err := resolveOutputDir(outDir)
	if err != nil {
		setStage("Erro", err.Error(), false)
		return "", err
	}
	title, err := getVideoTitle(ctx, url)
	if err != nil {
		title = "video-" + timestampName()
	}
	ext := outputExt(profile)
	outFile := filepath.Join(targetDir, title+ext)

	// 1) Baixa temporário
	setStage("Baixando", "Baixando vídeo original...", true)

	_ = os.RemoveAll(tempDir)
	_ = os.MkdirAll(tempDir, 0755)
	tempPattern := filepath.Join(tempDir, "video-temp.*")

	ytArgs := []string{"-o", filepath.Join(tempDir, "video-temp.%(ext)s"), url}
	ytCmd := exec.CommandContext(ctx, yt, ytArgs...)
	if log, runErr := runCmdWithLog(ytCmd); runErr != nil {
		if ctxCanceled(ctx) {
			cleanupConvertTemp(tempDir, tempPattern, outFile)
			return "", context.Canceled
		}
		setStage("Erro", fmt.Sprintf("yt-dlp falhou:\n%s", log), false)
		return "", fmt.Errorf("falha ao baixar (yt-dlp): %v\n%s", runErr, log)
	}

	inputFile, err := findFirstGlob(tempPattern)
	if err != nil {
		if ctxCanceled(ctx) {
			cleanupConvertTemp(tempDir, tempPattern, outFile)
			return "", context.Canceled
		}
		setStage("Erro", err.Error(), false)
		return "", err
	}

	// 2) Converter
	setStage("Convertendo", "Processando com ffmpeg...", true)

	var ffArgs []string
	ffArgs = append(ffArgs, "-hide_banner", "-loglevel", "info")
	if start != "" && end != "" {
		ffArgs = append(ffArgs, "-ss", start, "-to", end)
	}
	ffArgs = append(ffArgs, "-i", inputFile)
	ffArgs = append(ffArgs, ffmpegArgsPreset(profile)...)
	if strings.ToLower(profile) != "mp3" {
		ffArgs = append(ffArgs, "-avoid_negative_ts", "make_zero")
	}
	ffArgs = append(ffArgs, "-y", outFile)

	ffCmd := exec.CommandContext(ctx, ff, ffArgs...)
	if log, runErr := runCmdWithLog(ffCmd); runErr != nil {
		if ctxCanceled(ctx) {
			cleanupConvertTemp(tempDir, tempPattern, outFile)
			return "", context.Canceled
		}
		// fallback: sem corte
		if start != "" && end != "" {
			ffArgs2 := []string{"-hide_banner", "-loglevel", "info", "-i", inputFile}
			ffArgs2 = append(ffArgs2, ffmpegArgsPreset(profile)...)
			if strings.ToLower(profile) != "mp3" {
				ffArgs2 = append(ffArgs2, "-avoid_negative_ts", "make_zero")
			}
			ffArgs2 = append(ffArgs2, "-y", outFile)
			ffCmd2 := exec.CommandContext(ctx, ff, ffArgs2...)
			if log2, runErr2 := runCmdWithLog(ffCmd2); runErr2 == nil {
				_ = os.Remove(inputFile)
				_ = os.RemoveAll(tempDir)
				openInExplorerSelect(outFile)
				setStage("Finalizado", "Conversão concluída (fallback sem corte).", false)
				return outFile, nil
			} else {
				errMsg := fmt.Sprintf("ffmpeg falhou.\n---LOG 1 (com corte)---\n%s\n---LOG 2 (sem corte)---\n%s", log, log2)
				setStage("Erro", errMsg, false)
				cleanupConvertTemp(tempDir, tempPattern, outFile)
				return "", fmt.Errorf(errMsg)
			}
		}
		errMsg := fmt.Sprintf("ffmpeg falhou:\n%s", log)
		setStage("Erro", errMsg, false)
		cleanupConvertTemp(tempDir, tempPattern, outFile)
		return "", fmt.Errorf(errMsg)
	}

	// sucesso: remove temporários
	_ = os.Remove(inputFile)
	_ = os.RemoveAll(tempDir)

	openInExplorerSelect(outFile)
	setStage("Finalizado", "Conversão concluída com sucesso.", false)
	return outFile, nil
}

// ------------------- WINDOWS: SELETOR DE PASTA -------------------

type pickFolderResp struct {
	Path string `json:"path"`
}

func pickFolderHandler(w http.ResponseWriter, r *http.Request) {
	if runtime.GOOS != "windows" {
		http.Error(w, "Seleção de pasta suportada apenas no Windows.", http.StatusNotImplemented)
		return
	}
	ps := `Add-Type -AssemblyName System.Windows.Forms; $fbd = New-Object System.Windows.Forms.FolderBrowserDialog; if($fbd.ShowDialog() -eq 'OK'){[Console]::Out.Write($fbd.SelectedPath)}`
	cmd := exec.Command("powershell", "-NoProfile", "-STA", "-Command", ps)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err != nil {
		http.Error(w, "Falha ao abrir seletor de pasta: "+out.String(), http.StatusInternalServerError)
		return
	}

	raw := out.Bytes()
	if len(raw) >= 2 && raw[0] == 0xFF && raw[1] == 0xFE {
		raw = raw[2:]
	}
	u16 := make([]uint16, 0, len(raw)/2)
	for i := 0; i+1 < len(raw); i += 2 {
		u16 = append(u16, binary.LittleEndian.Uint16(raw[i:]))
	}
	path := strings.TrimSpace(string(utf16.Decode(u16)))

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(pickFolderResp{Path: path})
}

// ------------------- HTTP -------------------

func actionHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "método não suportado", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()

	var req actionReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "payload inválido", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.URL) == "" {
		http.Error(w, "URL do vídeo é obrigatória", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.OutputDir) == "" {
		http.Error(w, "Selecione a pasta de destino.", http.StatusBadRequest)
		return
	}

	if getStatus().Running {
		http.Error(w, "Já existe uma tarefa em execução. Aguarde terminar.", http.StatusConflict)
		return
	}

	setStage("Aguardando", "Iniciando tarefa...", true)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	setCurrentCancel(cancel)
	defer func() {
		cancel()
		clearCurrentCancel()
	}()

	switch strings.ToLower(req.Action) {
	case "download":
		out, err := handleDownload(ctx, req.URL, req.OutputDir)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				http.Error(w, "Operação cancelada pelo usuário.", http.StatusRequestTimeout)
				return
			}
			http.Error(w, "Erro ao baixar: "+err.Error(), http.StatusInternalServerError)
			return
		}
		openInExplorerSelect(out)
		_, _ = w.Write([]byte("✅ Download concluído: " + filepath.Base(out)))
	case "convert":
		out, err := handleConvert(ctx, req.URL, req.Profile, req.Start, req.End, req.OutputDir)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				http.Error(w, "Operação cancelada pelo usuário.", http.StatusRequestTimeout)
				return
			}
			http.Error(w, "Erro na conversão: "+err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("✅ Conversão concluída: " + filepath.Base(out)))
	default:
		http.Error(w, "ação desconhecida", http.StatusBadRequest)
	}
}

func statusHandler(w http.ResponseWriter, r *http.Request) {
	st := getStatus()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(st)
}

// ------------------- MAIN -------------------

func main() {
	if _, err := fs.Stat(content, "web/index.html"); err != nil {
		fmt.Println("Erro: index.html não encontrado dentro do embed")
		return
	}

	addr := flag.String("addr", "127.0.0.1:8080", "endereço do servidor")
	flag.Parse()

	http.HandleFunc("/", indexHandler)
	http.HandleFunc("/action", actionHandler)
	http.HandleFunc("/status", statusHandler)
	http.HandleFunc("/pick-folder", pickFolderHandler)
	http.HandleFunc("/cancel", cancelHandler)

	go openBrowser("http://" + *addr)
	fmt.Println("🚀 Servidor rodando em http://" + *addr)
	if err := http.ListenAndServe(*addr, nil); err != nil {
		fmt.Println("Erro no servidor:", err)
	}
}
