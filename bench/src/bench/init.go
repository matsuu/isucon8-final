package bench

import (
	"crypto/tls"
	"log"
	"net/http"
	"os"
	"time"
)

func init() {
	var err error
	loc, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		log.Panicln(err)
	}
	time.Local = loc

	// 見せない内部ログ用
	log.SetFlags(log.Lshortfile | log.LstdFlags | log.Lmicroseconds)
	log.SetOutput(os.Stderr)
	http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{
		InsecureSkipVerify: true,
	}
}
