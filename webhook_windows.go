//+build !windows

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/adnanh/webhook/hook"

	"github.com/codegangsta/negroni"
	"github.com/gorilla/mux"

	fsnotify "gopkg.in/fsnotify.v1"
)

const (
	version = "2.3.4"
)

var (
	ip             = flag.String("ip", "", "ip the webhook should serve hooks on")
	port           = flag.Int("port", 9000, "port the webhook should serve hooks on")
	verbose        = flag.Bool("verbose", false, "show verbose output")
	hotReload      = flag.Bool("hotreload", false, "watch hooks file for changes and reload them automatically")
	hooksFilePath  = flag.String("hooks", "hooks.json", "path to the json file containing defined hooks the webhook should serve")
	hooksURLPrefix = flag.String("urlprefix", "hooks", "url prefix to use for served hooks (protocol://yourserver:port/PREFIX/:hook-id)")
	secure         = flag.Bool("secure", false, "use HTTPS instead of HTTP")
	cert           = flag.String("cert", "cert.pem", "path to the HTTPS certificate pem file")
	key            = flag.String("key", "key.pem", "path to the HTTPS certificate private key pem file")

	watcher *fsnotify.Watcher
	signals chan os.Signal

	hooks hook.Hooks
)

func init() {
	hooks = hook.Hooks{}

	flag.Parse()

	log.SetPrefix("[webhook] ")
	log.SetFlags(log.Ldate | log.Ltime)

	if !*verbose {
		log.SetOutput(ioutil.Discard)
	}

	log.Println("version " + version + " starting")

	// load and parse hooks
	log.Printf("attempting to load hooks from %s\n", *hooksFilePath)

	err := hooks.LoadFromFile(*hooksFilePath)

	if err != nil {
		log.Printf("couldn't load hooks from file! %+v\n", err)
	} else {
		log.Printf("loaded %d hook(s) from file\n", len(hooks))

		for _, hook := range hooks {
			log.Printf("\t> %s\n", hook.ID)
		}
	}
}

func main() {
	if *hotReload {
		// set up file watcher
		log.Printf("setting up file watcher for %s\n", *hooksFilePath)

		var err error

		watcher, err = fsnotify.NewWatcher()
		if err != nil {
			log.Fatal("error creating file watcher instance", err)
		}

		defer watcher.Close()

		go watchForFileChange()

		err = watcher.Add(*hooksFilePath)
		if err != nil {
			log.Fatal("error adding hooks file to the watcher", err)
		}
	}

	l := negroni.NewLogger()
	l.Logger = log.New(os.Stdout, "[webhook] ", log.Ldate|log.Ltime)

	negroniRecovery := &negroni.Recovery{
		Logger:     l.Logger,
		PrintStack: true,
		StackAll:   false,
		StackSize:  1024 * 8,
	}

	n := negroni.New(negroniRecovery, l)

	router := mux.NewRouter()

	var hooksURL string

	if *hooksURLPrefix == "" {
		hooksURL = "/{id}"
	} else {
		hooksURL = "/" + *hooksURLPrefix + "/{id}"
	}

	router.HandleFunc(hooksURL, hookHandler)

	n.UseHandler(router)

	if *secure {
		log.Printf("starting secure (https) webhook on %s:%d", *ip, *port)
		log.Fatal(http.ListenAndServeTLS(fmt.Sprintf("%s:%d", *ip, *port), *cert, *key, n))
	} else {
		log.Printf("starting insecure (http) webhook on %s:%d", *ip, *port)
		log.Fatal(http.ListenAndServe(fmt.Sprintf("%s:%d", *ip, *port), n))
	}

}

func hookHandler(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]

	matchedHooks := hooks.MatchAll(id)

	if matchedHooks != nil {
		log.Printf("%s got matched (%d time(s))\n", id, len(matchedHooks))

		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			log.Printf("error reading the request body. %+v\n", err)
		}

		// parse headers
		headers := valuesToMap(r.Header)

		// parse query variables
		query := valuesToMap(r.URL.Query())

		// parse body
		var payload map[string]interface{}

		contentType := r.Header.Get("Content-Type")

		if strings.Contains(contentType, "json") {
			decoder := json.NewDecoder(strings.NewReader(string(body)))
			decoder.UseNumber()

			err := decoder.Decode(&payload)

			if err != nil {
				log.Printf("error parsing JSON payload %+v\n", err)
			}
		} else if strings.Contains(contentType, "form") {
			fd, err := url.ParseQuery(string(body))
			if err != nil {
				log.Printf("error parsing form payload %+v\n", err)
			} else {
				payload = valuesToMap(fd)
			}
		}

		// handle hook
		for _, h := range matchedHooks {
			h.ParseJSONParameters(&headers, &query, &payload)
			if h.TriggerRule == nil || h.TriggerRule != nil && h.TriggerRule.Evaluate(&headers, &query, &payload, &body) {
				log.Printf("%s hook triggered successfully\n", h.ID)

				if !h.CaptureCommandOutput {
					go handleHook(h, &headers, &query, &payload, &body)
					fmt.Fprintf(w, h.ResponseMessage)
				} else {
					response := handleHook(h, &headers, &query, &payload, &body)
					fmt.Fprintf(w, response)
				}

				return
			}
		}

		// if none of the hooks got triggered
		log.Printf("%s got matched (%d time(s)), but didn't get triggered because the trigger rules were not satisfied\n", matchedHooks[0].ID, len(matchedHooks))

		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "Hook rules were not satisfied.")
	} else {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, "Hook not found.")
	}
}

func handleHook(h *hook.Hook, headers, query, payload *map[string]interface{}, body *[]byte) string {
	cmd := exec.Command(h.ExecuteCommand)
	cmd.Args = h.ExtractCommandArguments(headers, query, payload)
	cmd.Dir = h.CommandWorkingDirectory

	log.Printf("executing %s (%s) with arguments %s using %s as cwd\n", h.ExecuteCommand, cmd.Path, cmd.Args, cmd.Dir)

	out, err := cmd.CombinedOutput()

	log.Printf("command output: %s\n", out)

	var errorResponse string

	if err != nil {
		log.Printf("error occurred: %+v\n", err)
		errorResponse = fmt.Sprintf("%+v", err)
	}

	log.Printf("finished handling %s\n", h.ID)

	var response []byte
	response, err = json.Marshal(&hook.CommandStatusResponse{ResponseMessage: h.ResponseMessage, Output: string(out), Error: errorResponse})

	if err != nil {
		log.Printf("error marshalling response: %+v", err)
		return h.ResponseMessage
	}

	return string(response)
}

func reloadHooks() {
	newHooks := hook.Hooks{}

	// parse and swap
	log.Printf("attempting to reload hooks from %s\n", *hooksFilePath)

	err := newHooks.LoadFromFile(*hooksFilePath)

	if err != nil {
		log.Printf("couldn't load hooks from file! %+v\n", err)
	} else {
		log.Printf("loaded %d hook(s) from file\n", len(hooks))

		for _, hook := range hooks {
			log.Printf("\t> %s\n", hook.ID)
		}

		hooks = newHooks
	}
}

func watchForFileChange() {
	for {
		select {
		case event := <-(*watcher).Events:
			if event.Op&fsnotify.Write == fsnotify.Write {
				log.Println("hooks file modified")

				reloadHooks()
			}
		case err := <-(*watcher).Errors:
			log.Println("watcher error:", err)
		}
	}
}

// valuesToMap converts map[string][]string to a map[string]string object
func valuesToMap(values map[string][]string) map[string]interface{} {
	ret := make(map[string]interface{})

	for key, value := range values {
		if len(value) > 0 {
			ret[key] = value[0]
		}
	}

	return ret
}
