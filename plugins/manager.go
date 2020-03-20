package plugins

import (
	"archive/zip"
	"errors"
	"fmt"
	"github.com/emqx/kuiper/common"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"unicode"
)

type Plugin struct {
	Name     string `json:"name"`
	File     string `json:"file"`
	Callback string `json:"callback"`
}

type PluginType int

const (
	SOURCE PluginType = iota
	SINK
	FUNCTION
)

var (
	pluginFolders = []string{"sources", "sinks", "functions"}
	once          sync.Once
	singleton     *Manager
)

//Registry is append only because plugin cannot delete or reload. To delete a plugin, restart the server to reindex
type Registry struct {
	sync.RWMutex
	internal [][]string
}

func (rr *Registry) Store(t PluginType, value string) {
	rr.Lock()
	rr.internal[t] = append(rr.internal[t], value)
	rr.Unlock()
}

func (rr *Registry) List(t PluginType) (values []string) {
	rr.RLock()
	result := rr.internal[t]
	rr.RUnlock()
	return result
}

//func (rr *Registry) Delete(t PluginType, value string) {
//	rr.Lock()
//	s := rr.internal[t]
//	for i, f := range s{
//		if f == value{
//			s[len(s)-1], s[i] = s[i], s[len(s)-1]
//			rr.internal[t] = s
//			break
//		}
//	}
//	rr.Unlock()
//}

type Manager struct {
	pluginDir string
	etcDir    string
	registry  *Registry
}

func NewPluginManager() (*Manager, error) {
	var err error
	once.Do(func() {
		dir, err := common.GetLoc("/plugins")
		if err != nil {
			err = fmt.Errorf("cannot find plugins folder: %s", err)
			return
		}
		etcDir, err := common.GetLoc("/etc")
		if err != nil {
			err = fmt.Errorf("cannot find etc folder: %s", err)
			return
		}

		plugins := make([][]string, 3)
		for i := 0; i < 3; i++ {
			names, err := findAll(PluginType(i), dir)
			if err != nil {
				err = fmt.Errorf("fail to find existing plugins: %s", err)
				return
			}
			plugins[i] = names
		}
		registry := &Registry{internal: plugins}

		singleton = &Manager{
			pluginDir: dir,
			etcDir:    etcDir,
			registry:  registry,
		}
	})
	return singleton, err
}

func findAll(t PluginType, pluginDir string) (result []string, err error) {
	dir := path.Join(pluginDir, pluginFolders[t])
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		return
	}

	for _, file := range files {
		baseName := filepath.Base(file.Name())
		if strings.HasSuffix(baseName, ".so") {
			result = append(result, lcFirst(baseName[0:len(baseName)-3]))
		}
	}
	return
}

func (m *Manager) List(t PluginType) (result []string, err error) {
	return m.registry.List(t), nil
}

func (m *Manager) Register(t PluginType, j *Plugin) error {
	name, uri, cb := j.Name, j.File, j.Callback
	//Validation
	name = strings.Trim(name, " ")
	if name == "" {
		return fmt.Errorf("invalid name %s: should not be empty", name)
	}
	if !isValidUrl(uri) || !strings.HasSuffix(uri, ".zip") {
		return fmt.Errorf("invalid uri %s", uri)
	}

	for _, n := range m.registry.List(t) {
		if n == name {
			return fmt.Errorf("invalid name %s: duplicate", name)
		}
	}
	zipPath := path.Join(m.pluginDir, name+".zip")
	var unzipFiles []string
	//clean up: delete zip file and unzip files in error
	defer os.Remove(zipPath)
	//download
	err := downloadFile(zipPath, uri)
	if err != nil {
		return fmt.Errorf("fail to download file %s: %s", uri, err)
	}
	//unzip and copy to destination
	unzipFiles, err = m.unzipAndCopy(t, name, zipPath)
	if err != nil {
		if t == SOURCE && len(unzipFiles) == 1 { //source that only copy so file
			os.Remove(unzipFiles[0])
		}
		return fmt.Errorf("fail to unzip file %s: %s", uri, err)
	}

	m.registry.Store(t, name)
	return callback(cb)
}

func (m *Manager) Delete(t PluginType, name string) (result error) {
	name = strings.Trim(name, " ")
	if name == "" {
		return fmt.Errorf("invalid name %s: should not be empty", name)
	}
	found := false
	for _, n := range m.registry.List(t) {
		if n == name {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("invalid name %s: not exist", name)
	}
	var results []string
	paths := []string{
		path.Join(m.pluginDir, pluginFolders[t], ucFirst(name)+".so"),
	}
	if t == SOURCE {
		paths = append(paths, path.Join(m.etcDir, pluginFolders[t], name+".yaml"))
	}
	for _, p := range paths {
		_, err := os.Stat(p)
		if err == nil {
			err = os.Remove(p)
			if err != nil {
				results = append(results, err.Error())
			}
		} else {
			results = append(results, fmt.Sprintf("can't find %s", p))
		}
	}

	if len(results) > 0 {
		return errors.New(strings.Join(results, "\n"))
	} else {
		return nil
	}
}

func (m *Manager) unzipAndCopy(t PluginType, name string, src string) ([]string, error) {
	var filenames []string
	r, err := zip.OpenReader(src)
	if err != nil {
		return filenames, err
	}
	defer r.Close()

	files := []string{
		ucFirst(name) + ".so",
	}
	paths := []string{
		path.Join(m.pluginDir, pluginFolders[t], files[0]),
	}
	if t == SOURCE {
		files = append(files, name+".yaml")
		paths = append(paths, path.Join(m.etcDir, pluginFolders[t], files[1]))
	}
	for i, d := range files {
		var z *zip.File
		for _, file := range r.File {
			fileName := file.Name
			if fileName == d {
				z = file
			}
		}
		if z == nil {
			return filenames, fmt.Errorf("invalid zip file: so file or conf file is missing")
		}

		err = unzipTo(z, paths[i])
		if err != nil {
			return filenames, err
		}
		filenames = append(filenames, paths[i])
	}
	return filenames, nil
}

func unzipTo(f *zip.File, fpath string) error {
	_, err := os.Stat(fpath)
	if err == nil || !os.IsNotExist(err) {
		return fmt.Errorf("%s already exist", fpath)
	}

	if f.FileInfo().IsDir() {
		return fmt.Errorf("%s: not a file, but a directory", fpath)
	}

	if err := os.MkdirAll(filepath.Dir(fpath), os.ModePerm); err != nil {
		return err
	}

	outFile, err := os.OpenFile(fpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
	if err != nil {
		return err
	}

	rc, err := f.Open()
	if err != nil {
		return err
	}

	_, err = io.Copy(outFile, rc)

	outFile.Close()
	rc.Close()
	return err
}

func isValidUrl(uri string) bool {
	_, err := url.ParseRequestURI(uri)
	if err != nil {
		return false
	}

	u, err := url.Parse(uri)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}

	return true
}

func downloadFile(filepath string, url string) error {

	// Get the data
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Create the file
	out, err := os.Create(filepath)
	if err != nil {
		return err
	}
	defer out.Close()

	// Write the body to file
	_, err = io.Copy(out, resp.Body)
	return err
}

func ucFirst(str string) string {
	for i, v := range str {
		return string(unicode.ToUpper(v)) + str[i+1:]
	}
	return ""
}

func lcFirst(str string) string {
	for i, v := range str {
		return string(unicode.ToLower(v)) + str[i+1:]
	}
	return ""
}

func callback(u string) error {
	return nil
}
