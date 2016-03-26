package command

import (
	"os/exec"
	"fmt"
	"runtime"
	"errors"
	"strings"
	"os"
	"io/ioutil"
	"path"
	"net/http"
	"io"
	"github.com/shyiko/jabba/cfg"
	"github.com/shyiko/jabba/semver"
	log "github.com/Sirupsen/logrus"
	"regexp"
	"github.com/mitchellh/ioprogress"
	"sort"
	"archive/zip"
)

func Install(selector string) (string, error) {
	var releaseMap map[*semver.Version]string
	var ver *semver.Version
	var err error
	// selector can be in form of <version>=<url>
	if strings.Contains(selector, "=") {
		split := strings.SplitN(selector, "=", 2)
		selector = split[0]
		// <version> has to be valid per semver
		ver, err = semver.ParseVersion(selector)
		if err != nil {
			return "", err
		}
		releaseMap = map[*semver.Version]string{ver: split[1]}
	} else {
		// ... or a version (range will be tried over remote targets)
		ver, _ = semver.ParseVersion(selector)
	}
	// check whether requested version is already installed
	if ver != nil {
		local, err := Ls()
		if err != nil {
			return "", err
		}
		for _, v := range local {
			if ver.Equals(v) {
				return ver.String(), nil
			}
		}
	}
	// ... apparently it's not
	if releaseMap == nil {
		ver = nil
		rng, err := semver.ParseRange(selector)
		if err != nil {
			return "", err
		}
		releaseMap, err = LsRemote()
		if err != nil {
			return "", err
		}
		var vs = make([]*semver.Version, len(releaseMap))
		var i = 0
		for k := range releaseMap {
			vs[i] = k
			i++
		}
		sort.Sort(sort.Reverse(semver.VersionSlice(vs)))
		for _, v := range vs {
			if rng.Contains(v) {
				ver = v
				break
			}
		}
		if ver == nil {
			tt := make([]string, len(vs))
			for i, v := range vs {
				tt[i] = v.String()
			}
			return "", errors.New("No compatible version found for " + selector +
			"\nValid install targets: " + strings.Join(tt, ", "))
		}
	}
	url := releaseMap[ver]
	if matched, _ := regexp.MatchString("^\\w+[+]\\w+://", url); !matched {
		return "", errors.New("URL must contain qualifier, e.g. tgz+http://...")
	}
	var fileType string = url[0:strings.Index(url, "+")]
	url = url[strings.Index(url, "+") + 1:]
	var file string
	var deleteFileWhenFinnished bool
	if strings.HasPrefix(url, "file://") {
		file = strings.TrimPrefix(url, "file://")
	} else {
		log.Info("Downloading ", ver, " (", url, ")")
		file, err = download(url)
		if err != nil {
			return "", err
		}
		deleteFileWhenFinnished = true
	}
	switch runtime.GOOS {
	case "darwin":
		err = installOnDarwin(ver.String(), file, fileType)
	case "linux":
		err = installOnLinux(ver.String(), file, fileType)
	default:
		err = errors.New(runtime.GOOS + " OS is not supported")
	}
	if err == nil && deleteFileWhenFinnished {
		os.Remove(file)
	}
	return ver.String(), err
}

type RedirectTracer struct {
	Transport http.RoundTripper
}

func (self RedirectTracer) RoundTrip(req *http.Request) (resp *http.Response, err error) {
	transport := self.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	resp, err = transport.RoundTrip(req)
	if err != nil {
		return
	}
	switch resp.StatusCode {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect:
		log.Debug("Following ", resp.StatusCode, " redirect to ", resp.Header.Get("Location"))
	}
	return
}

func download(url string) (file string, err error) {
	tmp, err := ioutil.TempFile("", "jabba-d-")
	if err != nil {
		return
	}
	file = tmp.Name()
	log.Debug("Saving ", url, " to ", file)
	// todo: timeout
	client := http.Client{Transport: RedirectTracer{}}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("too many redirects")
		}
		if len(via) != 0 {
			// https://github.com/golang/go/issues/4800
			for attr, val := range via[0].Header {
				if _, ok := req.Header[attr]; !ok {
					req.Header[attr] = val
				}
			}
		}
		return nil
	}
	req, err := http.NewRequest("GET", url, nil)
	req.Header.Set("Cookie", "oraclelicense=accept-securebackup-cookie")
	res, err := client.Do(req)
	if err != nil {
		return
	}
	defer res.Body.Close()
	progressTracker := &ioprogress.Reader{
		Reader: res.Body,
		Size: res.ContentLength,
	}
	_, err = io.Copy(tmp, progressTracker)
	if err != nil {
		return
	}
	return
}

func installOnDarwin(ver string, file string, fileType string) (err error) {
	target := cfg.Dir() + "/jdk/" + ver
	switch fileType {
	case "dmg":
		err = installFromDmg(file, target)
	case "zip":
		err = installFromZip(file, target + "/Contents/Home")
	default:
		return errors.New(fileType + " is not supported")
	}
	if err == nil {
		err = assertContentIsValid(target + "/Contents/Home")
	}
	if err != nil {
		os.RemoveAll(target)
	}
	return
}

func installFromDmg(source string, target string) error {
	tmp, err := ioutil.TempDir("", "jabba-i-")
	if err != nil {
		return err
	}
	basename := path.Base(source)
	mountpoint := tmp + "/" + basename
	pkgdir := tmp + "/" + basename + "-pkg"
	err = executeInShell([][]string{
		[]string{"Mounting " + source, "hdiutil mount -mountpoint " + mountpoint + " " + source},
		[]string{"Extracting " + source + " to " + target,
			"pkgutil --expand " + mountpoint + "/*.pkg " + pkgdir},
		[]string{"", "mkdir -p " + target},

		// todo: instead of relying on a certain pkg structure - find'n'extract all **/*/Payload

		// oracle
		[]string{"",
			"if [ -f " + pkgdir + "/jdk*.pkg/Payload" + " ]; then " +
			"tar xvf " + pkgdir + "/jdk*.pkg/Payload -C " + target +
			"; fi"},

		// apple
		[]string{"",
			"if [ -f " + pkgdir + "/JavaForOSX.pkg/Payload" + " ]; then " +
			"tar xzf " + pkgdir + "/JavaForOSX.pkg/Payload -C " + pkgdir + " &&" +
			"mv " + pkgdir + "/Library/Java/JavaVirtualMachines/*/Contents " + target + "/Contents" +
			"; fi"},

		[]string{"Unmounting " + source, "hdiutil unmount " + mountpoint},
	})
	if err == nil {
		os.RemoveAll(tmp)
	}
	return err
}

func installOnLinux(ver string, file string, fileType string) (err error) {
	target := cfg.Dir() + "/jdk/" + ver
	switch fileType {
	case "bin":
		err = installFromBin(file, target)
	case "tgz":
		err = installFromTgz(file, target)
	case "zip":
		err = installFromZip(file, target)
	default:
		return errors.New(fileType + " is not supported")
	}
	if err == nil {
		err = assertContentIsValid(target)
	}
	if err != nil {
		os.RemoveAll(target)
	}
	return
}

func installFromBin(source string, target string) (err error) {
	tmp, err := ioutil.TempDir("", "jabba-i-")
	if err != nil {
		return
	}
	err = executeInShell([][]string{
		[]string{"", "mv " + source + " " + tmp},
		[]string{"Extracting " + path.Join(tmp, path.Base(source)) + " to " + target,
			"cd " + tmp + " && echo | sh " + path.Base(source) + " && mv jdk*/ " + target},
	})
	if err == nil {
		os.RemoveAll(tmp)
	}
	return
}

func installFromTgz(source string, target string) error {
	return executeInShell([][]string{
		[]string{"", "mkdir -p " + target},
		[]string{"Extracting " + source + " to " + target,
			"tar xvf " + source + " --strip-components=1 -C " + target},
	})
}

func installFromZip(source string, target string) error {
	log.Info("Extracting " + source + " to " + target)
	return unzip(source, target, true)
}

func unzip(source string, target string, strip bool) error {
	r, err := zip.OpenReader(source)
	if err != nil {
		return err
	}
	defer r.Close()
	var prefixToStrip = ""
	if strip {
		entriesPerLevel := make(map[int]int)
		prefixMap := make(map[int]string)
		for _, f := range r.File {
			level := 0
			for _, c := range f.Name {
				if c == '/' {
					level++
				}
			}
			if !f.Mode().IsDir() {
				level++
			} else {
				prefixMap[level] = f.Name
			}
			entriesPerLevel[level]++
		}
		for i := 0; i < len(entriesPerLevel); i++ {
			if entriesPerLevel[i] > 1 && i > 0 {
				prefixToStrip = prefixMap[i - 1]
				break
			}
		}
	}
	for _, f := range r.File {
		name := strings.TrimPrefix(f.Name, prefixToStrip)
		if f.Mode().IsDir() {
			os.MkdirAll(path.Join(target, name), 0755)
		} else {
			fr, err := f.Open()
			if err != nil {
				return err
			}
			f, err := os.OpenFile(path.Join(target, name), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
			if err != nil {
				return err
			}
			_, err = io.Copy(f, fr)
			if err != nil {
				return err
			}
			f.Close()
		}
	}
	return nil
}

func executeInShell(cmd [][]string) error {
	for _, command := range cmd {
		if command[0] != "" {
			log.Info(command[0])
		}
		out, err := exec.Command("sh", "-c", command[1]).CombinedOutput()
		if err != nil {
			log.Error(string(out))
			return errors.New("'" + command[1] + "' failed: " + err.Error())
		}
	}
	return nil
}

func assertContentIsValid(target string) error {
	var err error
	if _, err = os.Stat(target + "/bin/java"); os.IsNotExist(err) {
		err = errors.New("<target>/bin/java wasn't found. " +
		"If you believe this is an error - please create a ticket at https://github.com/shyiko/jabba/issue " +
		"(specify OS and version/URL you tried to install)")
	}
	return err
}