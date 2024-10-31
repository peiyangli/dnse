package main

import (
	"bufio"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log"
	"os"
	"strconv"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/joho/godotenv"
)

func main() {
	log.SetFlags(11)
	pub, err := readPubKey("./pub.pem")
	if err != nil {
		log.Println(err)
		return
	}
	key := telegram.PublicKey{
		RSA: pub,
	}
	// https://core.telegram.org/api/obtaining_api_id

	// Using ".env" file to load environment variables.
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		return
	}

	// APP_HASH, APP_ID is from https://my.telegram.org/.
	appID, err := strconv.Atoi(os.Getenv("APP_ID"))
	if err != nil {
		return
	}
	appHash := os.Getenv("APP_HASH")
	if appHash == "" {
		return
	}

	client := telegram.NewClient(appID, appHash, telegram.Options{
		PublicKeys: []telegram.PublicKey{key},
	})
	if err := client.Run(context.Background(), func(ctx context.Context) error {
		// It is only valid to use client while this function is not returned
		// and ctx is not cancelled.
		api := client.API()

		// Now you can invoke MTProto RPC requests by calling the API.
		// ...
		phoneNumber := ""
		resSendCode, err := api.AuthSendCode(context.Background(), &tg.AuthSendCodeRequest{
			PhoneNumber: phoneNumber,
			APIID:       appID,
			APIHash:     appHash,
			Settings:    tg.CodeSettings{},
		})
		if err != nil {
			log.Println(err)
			return err
		}
		api.AuthSignIn(context.Background(), &tg.AuthSignInRequest{
			PhoneNumber:   phoneNumber,
			PhoneCodeHash: resSendCode.String(),
		})

		// Return to close client connection and free up resources.
		return nil
	}); err != nil {
		panic(err)
	}
	// Client is closed.
}

func readPubKey(fn string) (pubKey *rsa.PublicKey, err error) {
	pubKeyFile, err := os.Open(fn)
	if err != nil {
		fmt.Println(err)
		return nil, err
	}
	defer pubKeyFile.Close()
	pemfileinfo, _ := pubKeyFile.Stat()
	var size int64 = pemfileinfo.Size()
	pembytes := make([]byte, size)
	buffer := bufio.NewReader(pubKeyFile)
	_, err = buffer.Read(pembytes)
	if err != nil {
		return nil, err
	}
	data, _ := pem.Decode([]byte(pembytes))
	if err != nil {
		return nil, err
	}
	pubKey, err = x509.ParsePKCS1PublicKey(data.Bytes)
	return pubKey, err
}
