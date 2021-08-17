package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/pkg/errors"
	"github.com/skip2/go-qrcode"
)

const (
	UserAgent = "Mozilla/5.0 (iPhone; CPU iPhone OS 14_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148 KAKAOTALK 9.4.2"

	DefaultPngSize = 256

	TokenExpires = time.Second * 14
)

type Config struct {
	KakaoId string `json:"kakao-id"`
	KakaoPw string `json:"kakao-pw"`
	ApiKey  string `json:"api-key"`
	Bind    string `json:"bind"`
	Proxy   string `json:"proxy"`
}

var (
	cfg Config

	tokenCache        string
	tokenCachePng     = make(map[int][]byte)
	tokenCacheExpires time.Time
	tokenCacheLock    sync.Mutex

	httpClient http.Client
)

func main() {
	cfg = loadConfig()

	dpUserDir, err := ioutil.TempDir("", "tmp-kakaoqr-userdata-*")
	panicn(err)
	defer os.RemoveAll(dpUserDir)

	if cfg.Proxy != "" {
		if tcpProxy, err := net.DialTimeout("tcp", cfg.Proxy, time.Second); err == nil {
			tcpProxy.Close()

			proxyURL, _ := url.Parse("http://" + cfg.Proxy)
			httpClient.Transport = &http.Transport{
				Proxy: http.ProxyURL(proxyURL),
			}
		}
	}

	// Init Chromium
	dpOpts := []chromedp.ExecAllocatorOption{
		chromedp.UserDataDir(dpUserDir),
		chromedp.UserAgent(UserAgent),
		chromedp.WindowSize(375, 667),
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.Headless,
		chromedp.DisableGPU,
	}
	if cfg.Proxy != "" {
		dpOpts = append(dpOpts, chromedp.ProxyServer(cfg.Proxy))
	}
	dpContext, cancel := chromedp.NewExecAllocator(context.Background(), dpOpts...)
	defer cancel()

	var l net.Listener
	if _, _, err = net.SplitHostPort(cfg.Bind); err == nil {
		l, err = net.Listen("tcp", cfg.Bind)
	} else {
		if _, err := os.Stat(cfg.Bind); !os.IsNotExist(err) {
			err = os.Remove(cfg.Bind)
			panicn(err)
		}

		log.Printf("unix : %s\n", cfg.Bind)
		l, err = net.Listen("unix", cfg.Bind)
		panicn(err)

		err = os.Chmod(cfg.Bind, os.FileMode(0777))
	}
	panicn(err)
	defer l.Close()

	client := http.Server{
		BaseContext: func(net.Listener) context.Context { return dpContext },
		Handler:     http.HandlerFunc(serve),
	}

	go client.Serve(l)

	s := make(chan os.Signal, 1)
	signal.Notify(s, os.Interrupt, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	<-s
}

func resetCookieJar() {
	jar, _ := cookiejar.New(nil)
	httpClient.Jar = jar
}

func loadConfig() (cfg Config) {
	fs, err := os.Open("config.json")
	if err != nil {
		panic(err)
	}
	defer fs.Close()

	err = json.NewDecoder(fs).Decode(&cfg)
	if err != nil {
		panic(err)
	}

	return
}

func serve(w http.ResponseWriter, r *http.Request) {
	var err error
	remoteAddr := r.Header.Get("X-Real-IP")
	if remoteAddr == "" {
		remoteAddr = r.RemoteAddr
		if s := strings.IndexRune(remoteAddr, ':'); s > 0 {
			remoteAddr = remoteAddr[:s]
		}
	}

	log.Println(remoteAddr, r.Method, r.RequestURI)
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	defer func() {
		if errRaw := recover(); errRaw != nil {
			log.Printf("%+v\n", errors.WithStack(errRaw.(error)))
			w.WriteHeader(http.StatusInternalServerError)
		}
	}()

	if auth := r.Header.Get("X-API-KEY"); auth != cfg.ApiKey {
		log.Println("API Key is incorrect. IP: " + remoteAddr)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	pngMode := false
	pngSize := DefaultPngSize

	switch r.FormValue("type") {
	case "png":
		pngMode = true
	case "txt":
	case "":
	default:
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if pngMode {
		if sizeStr := r.FormValue("size"); sizeStr != "" {
			size, err := strconv.Atoi(sizeStr)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			pngSize = size
		}
	}

	ctx := r.Context()

	tokenCacheLock.Lock()
	defer tokenCacheLock.Unlock()

	if tokenCacheExpires.Before(time.Now()) {
		for k := range tokenCachePng {
			delete(tokenCachePng, k)
		}

		tokenCache, err = generateToken(ctx, false)
		if err != nil {
			tokenCache, err = generateToken(ctx, true)
			panicn(err)
		}

		tokenCacheExpires = time.Now().Add(TokenExpires)
	}

	var bodyData []byte
	if pngMode {
		d, ok := tokenCachePng[pngSize]
		if !ok {
			d, err = qrcode.Encode(tokenCache, qrcode.Medium, pngSize)
			panicn(err)

			tokenCachePng[pngSize] = d
		}

		w.Header().Set("Content-Type", "image/png")

		bodyData = d
	} else {
		w.Header().Set("Content-Type", "text/plain")
		bodyData = s2b(tokenCache)
	}

	w.Header().Set("Content-Length", strconv.Itoa(len(bodyData)))
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header()
	w.WriteHeader(http.StatusOK)
	w.Write(bodyData)
}

func generateToken(ctx context.Context, doLogin bool) (string, error) {
	if doLogin {
		resetCookieJar()

		ctxChrome, cancel := chromedp.NewContext(ctx)
		defer cancel()

		chResponse := make(chan struct{}, 1)
		chromedp.ListenTarget(
			ctxChrome,
			func(ev interface{}) {
				switch ev := ev.(type) {
				case *network.EventResponseReceived:
					if strings.EqualFold(ev.Response.URL, "https://accounts.kakao.com/weblogin/account/info") {
						chResponse <- struct{}{}
					}
				}
			},
		)

		err := chromedp.Run(
			ctxChrome,

			network.Enable(),
			network.ClearBrowserCookies(),

			// Login
			chromedp.Navigate(`https://accounts.kakao.com/login?continue=https%3A%2F%2Faccounts.kakao.com%2Fweblogin%2Faccount%2Finfo`),

			chromedp.WaitVisible("#login-form", chromedp.ByID),

			chromedp.SetValue(`#id_email_2`, cfg.KakaoId),
			chromedp.SetValue(`#id_password_3`, cfg.KakaoPw),
			chromedp.Click(`form#login-form button.submit`, chromedp.NodeVisible),

			chromedp.ActionFunc(
				func(ctx context.Context) error {
					select {
					case <-chResponse:
					case <-ctx.Done():
						return ctx.Err()
					}

					cookies, err := network.GetAllCookies().Do(ctx)
					if err != nil {
						return err
					}

					httpCookies := make([]*http.Cookie, 0, len(cookies))

					for _, cookie := range cookies {
						c := &http.Cookie{
							Domain:   cookie.Domain,
							Expires:  time.Unix(int64(cookie.Expires), 0),
							HttpOnly: cookie.HTTPOnly,
							Name:     cookie.Name,
							Path:     cookie.Path,
							Secure:   cookie.Secure,
							Value:    cookie.Value,
						}
						switch cookie.SameSite {
						case "Strict":
							c.SameSite = http.SameSiteStrictMode
						case "Lax":
							c.SameSite = http.SameSiteLaxMode
						case "None":
							c.SameSite = http.SameSiteNoneMode
						default:
							c.SameSite = http.SameSiteDefaultMode
						}

						httpCookies = append(httpCookies, c)
					}

					u, _ := url.Parse("https://accounts.kakao.com/weblogin/account/info")
					httpClient.Jar.SetCookies(u, httpCookies)

					return nil
				},
			),
		)
		if err != nil {
			return "", err
		}
	}

	//////////////////////////////////////////////////////////////////////////////////////////

	req, _ := http.NewRequest(
		"POST",
		"https://vaccine-qr.kakao.com/api/v1/qr",
		nil,
	)
	req.Body = ioutil.NopCloser(strings.NewReader(`{"epURL":null}`))
	req.Header = http.Header{
		"User-Agent":   []string{UserAgent},
		"content-type": []string{"application/json;charset=utf-8"},
	}
	req = req.WithContext(ctx)

	res, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("api/v1/qr returned %d %s", res.StatusCode, res.Status)
	}

	var qr struct {
		QRData string `json:"qrData"`
	}

	err = json.NewDecoder(res.Body).Decode(&qr)
	if err != nil && err != io.EOF {
		return "", err
	}
	if qr.QRData == "" {
		return "", errors.New("qrData is empty")
	}

	return qr.QRData, nil
}

func panicn(err error, ignores ...error) {
	if err == nil || err == io.EOF {
		return
	}
	for _, e := range ignores {
		if e == err {
			return
		}
	}
	panic(err)
}
