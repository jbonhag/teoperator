package server

import (
	"crypto/md5"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/schollz/httpfileserver"
	"github.com/schollz/logger"
	log "github.com/schollz/logger"
	"github.com/schollz/teoperator/src/audiosegment"
	"github.com/schollz/teoperator/src/download"
	"github.com/schollz/teoperator/src/ffmpeg"
	"github.com/schollz/teoperator/src/models"
	"github.com/schollz/teoperator/src/op1"
	"github.com/schollz/teoperator/src/utils"
)

//go:embed static templates
var content embed.FS

const MaxBytesPerFile = 100000000
const ContentDirectory = "data/uploads"

// uploads keep track of parallel chunking
var uploadsHashLock sync.Mutex
var uploadsLock sync.Mutex
var uploadsHash map[string]string
var uploadsInProgress map[string]int
var uploadsFileNames map[string]string
var serverName string

var rootNoteToFrequency = map[string]float64{
	"A#": math.Pow(2.0, ((58.0-69.0)/12.0)) * 440.0,
	"B":  math.Pow(2.0, ((59.0-69.0)/12.0)) * 440.0,
	"C":  math.Pow(2.0, ((60.0-69.0)/12.0)) * 440.0,
	"C#": math.Pow(2.0, ((61.0-69.0)/12.0)) * 440.0,
	"D":  math.Pow(2.0, ((62.0-69.0)/12.0)) * 440.0,
	"D#": math.Pow(2.0, ((63.0-69.0)/12.0)) * 440.0,
	"E":  math.Pow(2.0, ((64.0-69.0)/12.0)) * 440.0,
	"F":  math.Pow(2.0, ((65.0-69.0)/12.0)) * 440.0,
	"F#": math.Pow(2.0, ((66.0-69.0)/12.0)) * 440.0,
	"G":  math.Pow(2.0, ((67.0-69.0)/12.0)) * 440.0,
	"G#": math.Pow(2.0, ((68.0-69.0)/12.0)) * 440.0,
	"A":  math.Pow(2.0, ((69.0-69.0)/12.0)) * 440.0,
}

func Run(port int, sname string) (err error) {
	serverName = sname
	// initialize chunking maps
	uploadsInProgress = make(map[string]int)
	uploadsFileNames = make(map[string]string)
	uploadsHash = make(map[string]string)

	os.Mkdir("data", os.ModePerm)
	os.MkdirAll(ContentDirectory, os.ModePerm)
	loadTemplates()
	log.Infof("listening on :%d", port)
	http.Handle("/static/", http.FileServer(http.FS(content)))
	http.HandleFunc("/data/", httpfileserver.New("/data/", "data/", httpfileserver.OptionNoCache(true)).Handle())
	http.HandleFunc("/", handler)
	http.ListenAndServe(fmt.Sprintf(":%d", port), nil)
	return
}

func handler(w http.ResponseWriter, r *http.Request) {
	t := time.Now().UTC()
	log.Infof("%v %v %v %s\n", r.RemoteAddr, r.Method, r.URL.Path, time.Since(t))
	err := handle(w, r)
	if err != nil {
		log.Error(err)
		viewMain(w, r, err.Error(), "main")
	}
	log.Infof("%v %v %v (%s) %s\n", r.RemoteAddr, r.Method, r.URL.Path, r.Header.Get("Accept-Language"), time.Since(t))
}

type Href struct {
	Value string
	Href  string
	Flag  bool
}

type Metadata struct {
	Name          string
	UUID          string
	OriginalURL   string
	Files         []FileData
	Start         float64
	Stop          float64
	IsSynthPatch  bool
	RemoveSilence bool
	RootNote      string
	Splices       int
}

type FileData struct {
	Prefix string
	Start  float64
	Stop   float64
}

type Render struct {
	Title        string
	MessageError string
	MessageInfo  string
	Metadata     Metadata
}

var t map[string]*template.Template
var mu sync.Mutex

func loadTemplates() {
	mu.Lock()
	defer mu.Unlock()
	t = make(map[string]*template.Template)
	funcMap := template.FuncMap{
		"beforeFirstComma": func(s string) string {
			ss := strings.Split(s, ",")
			if len(ss) == 1 {
				return s
			}
			if len(ss[0]) > 8 {
				return strings.TrimSpace(ss[0])
			}
			return strings.TrimSpace(ss[0] + ", " + ss[1])
		},
		"humanizeTime": func(t time.Time) string {
			return humanize.Time(t)
		},
		"add": func(a, b int) int {
			return a + b
		},
		"removeSlashes": func(s string) string {
			return strings.TrimPrefix(strings.TrimSpace(strings.Replace(s, "/", "-", -1)), "-location-")
		},
		"removeDots": func(s string) string {
			return strings.TrimSpace(strings.Replace(s, ".", "", -1))
		},
		"minusOne": func(s int) int {
			return s - 1
		},
		"mod": func(i, j int) bool {
			return i%j == 0
		},
		"urlbase": func(s string) string {
			uparsed, _ := url.Parse(s)
			return filepath.Base(uparsed.Path)
		},
		"filebase": func(s string) string {
			_, base := filepath.Split(s)
			base = strings.Replace(base, ".", "", -1)
			return base
		},
		"roundfloat": func(f float64) string {
			return fmt.Sprintf("%2.1f", f)
		},
	}
	for _, templateName := range []string{"main"} {
		b, err := content.ReadFile("templates/base.html")
		if err != nil {
			panic(err)
		}
		t[templateName] = template.Must(template.New("base").Funcs(funcMap).Delims("((", "))").Parse(string(b)))
		b, err = content.ReadFile("templates/" + templateName + ".html")
		if err != nil {
			panic(err)
		}
		t[templateName] = template.Must(t[templateName].Parse(string(b)))
		log.Tracef("loaded template %s", templateName)
	}

}

func handle(w http.ResponseWriter, r *http.Request) (err error) {
	if log.GetLevel() == "debug" || log.GetLevel() == "trace" {
		loadTemplates()
	}

	if r.URL.Path == "/ws" {
	} else if r.URL.Path == "/favicon.ico" {
		http.Redirect(w, r, "/static/img/favicon.ico", http.StatusFound)
	} else if r.URL.Path == "/robots.txt" {
		http.Redirect(w, r, "/static/robots.txt", http.StatusFound)
	} else if r.URL.Path == "/sitemap.xml" {
		http.Redirect(w, r, "/static/sitemap.xml", http.StatusFound)
	} else if r.URL.Path == "/" {
		return viewMain(w, r, "", "main")
	} else if r.URL.Path == "/file" {
		return handlePost(w, r)
	} else if r.URL.Path == "/patch" {
		return viewPatch(w, r)
	} else {
		t["main"].Execute(w, Render{})
	}

	return
}

func handlePost(w http.ResponseWriter, r *http.Request) (err error) {
	r.ParseMultipartForm(32 << 20)
	file, handler, errForm := r.FormFile("file")
	if errForm != nil {
		err = errForm
		log.Error(err)
		return err
	}
	defer file.Close()
	fname, _ := filepath.Abs(handler.Filename)
	_, fname = filepath.Split(fname)

	log.Debugf("%+v", r.Form)
	chunkNum, _ := strconv.Atoi(r.FormValue("dzchunkindex"))
	chunkNum++
	totalChunks, _ := strconv.Atoi(r.FormValue("dztotalchunkcount"))
	chunkSize, _ := strconv.Atoi(r.FormValue("dzchunksize"))
	if int64(totalChunks)*int64(chunkSize) > MaxBytesPerFile {
		err = fmt.Errorf("Upload exceeds max file size: %d.", MaxBytesPerFile)
		log.Error(err)
		jsonResponse(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
		return nil
	}
	uuid := r.FormValue("dzuuid")
	log.Debugf("working on chunk %d/%d for %s", chunkNum, totalChunks, uuid)

	f, err := ioutil.TempFile(ContentDirectory, "upload")
	if err != nil {
		log.Error(err)
		return
	}
	// remove temp file when finished
	_, err = CopyMax(f, file, MaxBytesPerFile)
	if err != nil {
		log.Error(err)
	}
	f.Close()

	// check if need to cat
	uploadsLock.Lock()
	if _, ok := uploadsInProgress[uuid]; !ok {
		uploadsInProgress[uuid] = 0
	}
	uploadsInProgress[uuid]++
	uploadsFileNames[fmt.Sprintf("%s%d", uuid, chunkNum)] = f.Name()
	if uploadsInProgress[uuid] == totalChunks {
		err = func() (err error) {
			log.Debugf("upload finished for %s", uuid)
			log.Debugf("%+v", uploadsFileNames)
			delete(uploadsInProgress, uuid)

			fFinal, _ := ioutil.TempFile(ContentDirectory, "upload")
			originalSize := int64(0)
			for i := 1; i <= totalChunks; i++ {
				// cat each chunk
				fh, err := os.Open(uploadsFileNames[fmt.Sprintf("%s%d", uuid, i)])
				delete(uploadsFileNames, fmt.Sprintf("%s%d", uuid, i))
				if err != nil {
					log.Error(err)
					return err
				}
				n, errCopy := io.Copy(fFinal, fh)
				originalSize += n
				if errCopy != nil {
					log.Error(errCopy)
				}
				fh.Close()
				log.Debugf("removed %s", fh.Name())
				os.Remove(fh.Name())
			}
			fFinal.Close()
			log.Debugf("final written to: %s", fFinal.Name())

			// rename
			logger.Debugf("renamed to %s", fFinal.Name()+fname)
			os.Rename(fFinal.Name(), fFinal.Name()+fname)

			log.Debugf("setting uploadsHash: %s", fFinal.Name()+fname)
			uploadsHashLock.Lock()
			uploadsHash[uuid] = fFinal.Name() + fname
			uploadsHashLock.Unlock()
			return
		}()
	}
	uploadsLock.Unlock()

	if err != nil {
		logger.Error(err)
		jsonResponse(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
		return nil
	}

	// wait until all are finished
	var finalFname string
	startTime := time.Now()
	for {
		uploadsHashLock.Lock()
		if _, ok := uploadsHash[uuid]; ok {
			finalFname = uploadsHash[uuid]
			log.Debugf("got uploadsHash: %s", finalFname)
		}
		uploadsHashLock.Unlock()
		if finalFname != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
		if time.Since(startTime).Seconds() > 60*60 {
			break
		}
	}

	// TODO: cleanup if last one, delete uuid from uploadshash
	_, finalFname = filepath.Split(finalFname)
	jsonResponse(w, http.StatusCreated, map[string]string{"id": fmt.Sprintf("%s/data/uploads/%s", serverName, finalFname)})
	return
}

func viewPatch(w http.ResponseWriter, r *http.Request) (err error) {
	audioURL, _ := r.URL.Query()["audioURL"]
	secondsStart, _ := r.URL.Query()["secondsStart"]
	secondsEnd, _ := r.URL.Query()["secondsEnd"]
	patchtypeA, _ := r.URL.Query()["synthPatch"]
	removeSilenceA, _ := r.URL.Query()["removeSilence"]
	rootNoteA, _ := r.URL.Query()["rootNote"]
	splicesA, _ := r.URL.Query()["splices"]
	patchtype := "drum"
	removeSilence := false
	rootNote := "A"
	if len(patchtypeA) > 0 && (patchtypeA[0] == "synth" || patchtypeA[0] == "on") {
		patchtype = "synth"
	}
	if len(removeSilenceA) > 0 && removeSilenceA[0] == "yes" {
		removeSilence = true
	}
	if len(rootNoteA) > 0 {
		if _, ok := rootNoteToFrequency[rootNoteA[0]]; ok {
			rootNote = rootNoteA[0]
		}
	}
	log.Debugf("removeSilence: %+v", removeSilence)

	if len(audioURL[0]) == 0 {
		err = fmt.Errorf("no URL")
		return
	}

	startStop := []float64{0, 0}
	if secondsStart[0] != "" {
		startStop[0], _ = strconv.ParseFloat(secondsStart[0], 64)
	}
	if secondsEnd[0] != "" {
		startStop[1], _ = strconv.ParseFloat(secondsEnd[0], 64)
	}
	splices, _ := strconv.Atoi(splicesA[0])
	log.Debugf("splices: %d", splices)

	uuid, err := generateUserData(audioURL[0], startStop, patchtype, removeSilence, rootNote, splices)
	if err != nil {
		return
	}

	metadatab, err := ioutil.ReadFile(path.Join("data", uuid, "metadata.json"))
	if err != nil {
		return
	}
	var metadata Metadata
	err = json.Unmarshal(metadatab, &metadata)
	if err != nil {
		return
	}

	t["main"].Execute(w, Render{
		Metadata: metadata,
	})
	return
}

func viewMain(w http.ResponseWriter, r *http.Request, messageError string, templateName string) (err error) {

	t[templateName].Execute(w, Render{
		Title:        "chop | make op-1 patches",
		MessageError: messageError,
	})
	return
}

func generateUserData(u string, startStop []float64, patchType string, removeSilence bool, rootNote string, splices int) (uuid string, err error) {
	log.Debug(u, startStop)
	log.Debug(patchType)
	if startStop[1]-startStop[0] < 12 {
		startStop[1] = startStop[0] + 12
	}
	if patchType != "drum" {
		startStop[1] = startStop[0] + 5.75
	}

	uuid = fmt.Sprintf("%x", md5.Sum([]byte(fmt.Sprintf("%+v %+v %+v %+v %+v %+v", patchType, u, startStop, removeSilence, rootNote, splices))))

	// create path to data
	pathToData := path.Join("data", uuid)

	_, errstat := os.Stat(pathToData)
	if errstat == nil {
		// already exists, done here
		return
	}

	err = os.Mkdir(pathToData, os.ModePerm)
	if err != nil {
		return
	}

	// find filename of downloaded file
	fname := ""
	uparsed, err := url.Parse(u)
	if err != nil {
		return
	}
	fname = path.Join(pathToData, path.Base(uparsed.Path))
	if !strings.Contains(fname, ".") {
		fname += ".wav"
	}

	fnameID := path.Join("data", fmt.Sprintf("%x%s", md5.Sum([]byte(u)), filepath.Ext(fname)))

	_, errstat = os.Stat(fnameID)
	var alternativeName string
	if errstat != nil {
		log.Debugf("downloading to %s", fnameID)
		alternativeName, err = download.Download(u, fnameID, 100000000)
		if err != nil {
			return
		}

	}

	folder0, _ := filepath.Split(fname)
	shortName := fmt.Sprintf("%x%s", md5.Sum([]byte(u+fmt.Sprintf("%+v", startStop))), filepath.Ext(fname))
	shortName = shortName[:6]
	shortName = path.Join(folder0, shortName+filepath.Ext(fname))

	// // copy file into folder
	// _, err = utils.CopyFile(fnameID, fname)
	// if err != nil {
	// 	return
	// }
	// truncate into folder
	err = audiosegment.Truncate(fnameID, shortName, utils.SecondsToString(startStop[0]), utils.SecondsToString(startStop[1]))
	if err != nil {
		log.Error(err)
		return
	}

	if removeSilence {
		log.Debug("removing silence")
		err = os.Rename(shortName, shortName+".wav")
		if err != nil {
			log.Error(err)
			return
		}
		err = ffmpeg.RemoveSilence(shortName+".wav", shortName)
		if err != nil {
			log.Error(err)
			return
		}
		err = os.Remove(shortName + ".wav")
		if err != nil {
			log.Error(err)
			return
		}
	}

	// remove upload if upload
	log.Debugf("u: %s", u)
	if strings.Contains(u, "data/uploads/upload") {
		errRemove := os.Remove(path.Join("data", "uploads", path.Base(uparsed.Path)))
		if errRemove != nil {
			log.Error(errRemove)
		} else {
			log.Debug("removed upload")
		}
	}

	// generate patches
	var segments [][]models.AudioSegment
	if patchType == "drum" {
		segments, err = audiosegment.SplitEqual(shortName, 12, 1, splices)
		if err != nil {
			return
		}
	} else {
		segments, err = makeSynthPatch(shortName, rootNoteToFrequency[rootNote])
		if err != nil {
			return
		}
	}

	// write metadata
	files := make([]FileData, len(segments))
	for i, seg := range segments {
		files[i] = FileData{
			Prefix: seg[0].Filename[:len(seg[0].Filename)-4],
			Start:  seg[0].StartAbs + startStop[0],
			Stop:   seg[0].EndAbs + startStop[0],
		}
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].Start < files[j].Start
	})

	log.Debug(alternativeName)
	if alternativeName != "" {
		fname = alternativeName
	}
	b, _ := json.Marshal(Metadata{
		Name:          fname,
		UUID:          uuid,
		OriginalURL:   u,
		Files:         files,
		Start:         startStop[0],
		Stop:          startStop[1],
		IsSynthPatch:  patchType == "synth",
		RemoveSilence: removeSilence,
		RootNote:      rootNote,
		Splices:       splices,
	})
	err = ioutil.WriteFile(path.Join(pathToData, "metadata.json"), b, 0644)

	return
}

func makeSynthPatch(fname string, rootFrequency float64) (segments [][]models.AudioSegment, err error) {
	sp := op1.NewSynthSamplePatch(rootFrequency)
	basefolder, basefname := filepath.Split(fname)
	sp.Name = strings.Split(basefname, ".")[0]
	fnameout := path.Join(basefolder, strings.Split(basefname, ".")[0]+".aif")

	err = sp.SaveSample(fname, fnameout, true)
	if err != nil {
		return
	}
	segments = [][]models.AudioSegment{
		[]models.AudioSegment{
			models.AudioSegment{
				Filename: fnameout,
				StartAbs: 0,
				EndAbs:   5.75,
			},
		},
	}

	fnamewav := path.Join(basefolder, strings.Split(basefname, ".")[0]+".wav")
	cmd := fmt.Sprintf("-y -i %s %s", fnameout, fnamewav)
	logger.Debug(cmd)
	out, err := exec.Command("ffmpeg", strings.Fields(cmd)...).CombinedOutput()
	logger.Debugf("ffmpeg: %s", out)
	if err != nil {
		err = fmt.Errorf("ffmpeg; %s", err.Error())
		return
	}

	waveformfname := fnamewav + ".png"
	cmd = fmt.Sprintf("-i %s -o %s --background-color ffffff00 --waveform-color ffffff --amplitude-scale 2 --no-axis-labels --pixels-per-second 100 --height 160 --width %2.0f",
		fnamewav, waveformfname, 5.75*100,
	)
	logger.Debug(cmd)
	out, err = exec.Command("audiowaveform", strings.Fields(cmd)...).CombinedOutput()
	if err != nil {
		logger.Errorf("audiowaveform: %s", out)
	}

	return
}

// CopyMax copies only the maxBytes and then returns an error if it
// copies equal to or greater than maxBytes (meaning that it did not
// complete the copy).
func CopyMax(dst io.Writer, src io.Reader, maxBytes int64) (n int64, err error) {
	n, err = io.CopyN(dst, src, maxBytes)
	if err != nil && err != io.EOF {
		return
	}

	if n >= maxBytes {
		err = fmt.Errorf("Upload exceeds maximum size (%d).", MaxBytesPerFile)
	} else {
		err = nil
	}
	return
}

// jsonResponse writes a JSON response and HTTP code
func jsonResponse(w http.ResponseWriter, code int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json, err := json.Marshal(data)
	if err != nil {
		log.Error(err)
	}
	log.Debugf("json response: %s", json)
	fmt.Fprintf(w, "%s\n", json)
}
