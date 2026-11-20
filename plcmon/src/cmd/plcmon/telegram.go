package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"k8s.io/klog/v2"
	"net/http"
	"net/url"
	"time"
)

var (
	telegramKey   = flag.String("telegramKey", "", "Telegram bot key")
	telegramGroup = flag.String("telegramGroup", "", "Telegram destination group ID")

	httpClient = http.Client{
		Timeout: 10 * time.Second,
	}
)

func validateTelegramArgs() {
	if *telegramKey != "" && *telegramGroup == "" {
		klog.Exit("Please specify -telegramGroup with -telegramKey")
	}
}

func sendTelegramMessage(body string) {
	go func() {
		message := map[string]string{
			"text":    body,
			"chat_id": *telegramGroup,
		}
		payload, err := json.Marshal(message)
		if err != nil {
			panic(err)
		}

		endpoint := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", *telegramKey)

		req, err := http.NewRequest("POST", endpoint, bytes.NewBuffer(payload))
		if err != nil {
			panic(err)
		}

		req.Header.Set("Content-Type", "application/json")

		resp, err := httpClient.Do(req)
		if err != nil {
			// A *url.Error includes the request URL, which contains the bot
			// key. Only log the underlying error.
			var urlErr *url.Error
			if errors.As(err, &urlErr) {
				err = urlErr.Err
			}
			klog.Errorf("error sending Telegram message: %v", err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			klog.Errorf("sendMessage returned status code %d", resp.StatusCode)
		}
	}()
}
