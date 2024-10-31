package main

import (
	"bufio"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	pebbledb "github.com/cockroachdb/pebble"
	"github.com/go-faster/errors"
	boltstor "github.com/gotd/contrib/bbolt"
	"github.com/gotd/contrib/middleware/floodwait"
	"github.com/gotd/contrib/middleware/ratelimit"
	"github.com/gotd/contrib/pebble"
	"github.com/gotd/contrib/storage"
	"github.com/joho/godotenv"
	"go.etcd.io/bbolt"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/net/proxy"
	"golang.org/x/sync/errgroup"
	"golang.org/x/time/rate"
	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/examples"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/telegram/message/peer"
	"github.com/gotd/td/telegram/query"
	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/tg"
)

func main() {

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	if err := run(ctx); err != nil {
		if errors.Is(err, context.Canceled) && ctx.Err() == context.Canceled {
			fmt.Println("\rClosed")
			os.Exit(0)
		}
		_, _ = fmt.Fprintf(os.Stderr, "Error: %+v\n", err)
		os.Exit(1)
	} else {
		fmt.Println("Done")
		os.Exit(0)
	}
}

func sessionFolder(phone string) string {
	var out []rune
	for _, r := range phone {
		if r >= '0' && r <= '9' {
			out = append(out, r)
		}
	}
	return "phone-" + string(out)
}

func run(ctx context.Context) error {
	var arg struct {
		FillPeerStorage bool
	}
	flag.BoolVar(&arg.FillPeerStorage, "fill-peer-storage", false, "fill peer storage")
	flag.Parse()

	// Using ".env" file to load environment variables.
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		return errors.Wrap(err, "load env")
	}

	// TG_PHONE is phone number in international format.
	// Like +4123456789.
	phone := os.Getenv("TG_PHONE")
	if phone == "" {
		return errors.New("no phone")
	}
	// APP_HASH, APP_ID is from https://my.telegram.org/.
	appID, err := strconv.Atoi(os.Getenv("APP_ID"))
	if err != nil {
		return errors.Wrap(err, " parse app id")
	}
	appHash := os.Getenv("APP_HASH")
	if appHash == "" {
		return errors.New("no app hash")
	}

	// Setting up session storage.
	// This is needed to reuse session and not login every time.
	sessionDir := filepath.Join("session", sessionFolder(phone))
	if err := os.MkdirAll(sessionDir, 0700); err != nil {
		return err
	}
	logFilePath := filepath.Join(sessionDir, "log.jsonl")

	fmt.Printf("Storing session in %s, logs in %s\n", sessionDir, logFilePath)

	// Setting up logging to file with rotation.
	//
	// Log to file, so we don't interfere with prompts and messages to user.
	logWriter := zapcore.AddSync(&lumberjack.Logger{
		Filename:   logFilePath,
		MaxBackups: 3,
		MaxSize:    1, // megabytes
		MaxAge:     7, // days
	})
	logCore := zapcore.NewCore(
		zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()),
		logWriter,
		zap.DebugLevel,
	)
	lg := zap.New(logCore)
	defer func() { _ = lg.Sync() }()

	// So, we are storing session information in current directory, under subdirectory "session/phone_hash"
	sessionStorage := &telegram.FileSessionStorage{
		Path: filepath.Join(sessionDir, "session.json"),
	}
	// Peer storage, for resolve caching and short updates handling.
	db, err := pebbledb.Open(filepath.Join(sessionDir, "peers.pebble.db"), &pebbledb.Options{})
	if err != nil {
		return errors.Wrap(err, "create pebble storage")
	}
	peerDB := pebble.NewPeerStorage(db)
	lg.Info("Storage", zap.String("path", sessionDir))

	// Setting up client.
	//
	// Dispatcher is used to register handlers for events.
	dispatcher := tg.NewUpdateDispatcher()
	// Setting up update handler that will fill peer storage before
	// calling dispatcher handlers.
	updateHandler := storage.UpdateHook(dispatcher, peerDB)

	// Setting up persistent storage for qts/pts to be able to
	// recover after restart.
	boltdb, err := bbolt.Open(filepath.Join(sessionDir, "updates.bolt.db"), 0666, nil)
	if err != nil {
		return errors.Wrap(err, "create bolt storage")
	}
	updatesRecovery := updates.New(updates.Config{
		Handler: updateHandler, // using previous handler with peerDB
		Logger:  lg.Named("updates.recovery"),
		Storage: boltstor.NewStateStorage(boltdb),
	})

	// Handler of FLOOD_WAIT that will automatically retry request.
	waiter := floodwait.NewWaiter().WithCallback(func(ctx context.Context, wait floodwait.FloodWait) {
		// Notifying about flood wait.
		lg.Warn("Flood wait", zap.Duration("wait", wait.Duration))
		fmt.Println("Got FLOOD_WAIT. Will retry after", wait.Duration)
	})
	fmt.Println(waiter)

	// pub, err := readPubKey("./pub.pem")
	// if err != nil {
	// 	return err
	// }
	// key := telegram.PublicKey{
	// 	RSA: pub,
	// }

	sock5, err := proxy.SOCKS5("tcp", "10.10.1.99:1080", &proxy.Auth{
		User:     "pei",
		Password: "batchat",
	}, proxy.Direct)
	if err != nil {
		fmt.Println("proxy error")
		panic(err)
	}
	dc := sock5.(proxy.ContextDialer)

	//read config
	var dcList = DCProd()
	dcCfgFileName := "./dcCfg.bin"
	{
		cfgBin, err := os.ReadFile(dcCfgFileName)
		if err != nil {
			fmt.Printf("no dcCfgFile: %v\n", err)
		} else {
			var b = bin.Buffer{Buf: cfgBin}
			var dcCfg tg.Config
			err = dcCfg.Decode(&b)
			if err != nil {
				fmt.Printf("dcCfgFile Decode err: %v\n", err)
			} else {
				if len(dcCfg.DCOptions) > 0 {
					fmt.Printf("use dcCfgFile: %v\n", dcCfg)
					dcList.Options = dcCfg.DCOptions
				}
			}
			os.Remove(dcCfgFileName)
		}
	}

	// Filling client options.
	options := telegram.Options{
		// PublicKeys: []telegram.PublicKey{key},
		Device: getDevice(),
		DCList: dcList, //dcs.Prod(), //
		Resolver: dcs.Plain(dcs.PlainOptions{
			Dial: dc.DialContext,
		}),
		Logger:         lg,              // Passing logger for observability.
		SessionStorage: sessionStorage,  // Setting up session sessionStorage to store auth data.
		UpdateHandler:  updatesRecovery, // Setting up handler for updates from server.
		Middlewares: []telegram.Middleware{
			// Setting up FLOOD_WAIT handler to automatically wait and retry request.
			// waiter,
			// Setting up general rate limits to less likely get flood wait errors.
			ratelimit.New(rate.Every(time.Millisecond*100), 5),
		},
	}
	client := telegram.NewClient(appID, appHash, options)

	// Registering handler for new private messages.
	var server = &Server{
		DBPeer:    peerDB,
		Downloads: make(chan DownloadInfo, 100),
	}
	dispatcher.OnNewChannelMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateNewChannelMessage) error {
		msg, ok := u.Message.(*tg.Message)
		if !ok {
			fmt.Printf("unknown msg: %v\n", u.Message)
			return nil
		}
		return server.onMessage(ctx, e, msg)
	})
	dispatcher.OnNewMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateNewMessage) error {
		msg, ok := u.Message.(*tg.Message)
		if !ok {
			fmt.Printf("unknown msg: %v\n", u.Message)
			return nil
		}
		return server.onMessage(ctx, e, msg)
	})

	// Authentication flow handles authentication process, like prompting for code and 2FA password.
	flow := auth.NewFlow(examples.Terminal{PhoneNumber: phone}, auth.SendCodeOptions{})

	var runner = func(ctx context.Context) error {
		// Spawning main goroutine.
		fmt.Println("waiter running...")

		api := client.API()
		// Setting up resolver cache that will use peer storage.
		resolver := storage.NewResolverCache(peer.Plain(api), peerDB)
		// Usage:
		//   if _, err := resolver.ResolveDomain(ctx, "tdlibchat"); err != nil {
		//	   return errors.Wrap(err, "resolve")
		//   }
		_ = resolver

		if err := client.Run(ctx, func(ctx context.Context) error {
			// Perform auth if no session is available.
			fmt.Println("client running...")
			if err := client.Auth().IfNecessary(ctx, flow); err != nil {
				return errors.Wrap(err, "auth")
			}

			// Getting info about current user.
			self, err := client.Self(ctx)
			if err != nil {
				return errors.Wrap(err, "call self")
			}

			name := self.FirstName
			if self.Username != "" {
				// Username is optional.
				name = fmt.Sprintf("%s (@%s)", name, self.Username)
			}
			fmt.Println("Current user:", name)

			lg.Info("Login",
				zap.String("first_name", self.FirstName),
				zap.String("last_name", self.LastName),
				zap.String("username", self.Username),
				zap.Int64("id", self.ID),
			)

			if arg.FillPeerStorage {
				fmt.Println("Filling peer storage from dialogs to cache entities")
				collector := storage.CollectPeers(peerDB)
				if err := collector.Dialogs(ctx, query.GetDialogs(api).Iter()); err != nil {
					return errors.Wrap(err, "collect peers")
				}
				fmt.Println("Filled")
			}

			if true {
				fmt.Println("api.HelpGetConfig...")
				dcCfg, err := api.HelpGetConfig(ctx)
				if err != nil {
					fmt.Printf("HelpGetConfig failed: %v\n", err)
				} else {
					fmt.Printf("HelpGetConfig: %v\n", dcCfg)
					var b bin.Buffer
					err = dcCfg.Encode(&b)
					if err != nil {
						fmt.Printf("HelpGetConfig Encode %v", err)
					} else {
						os.WriteFile(dcCfgFileName, b.Buf, 0666)
					}
				}
			}
			api = client.API()
			go func() {
				jobs := 5
				g, ctx := errgroup.WithContext(ctx)

				for j := 0; j < jobs; j++ {
					g.Go(func() error {
						// Process all discovered downloads.
						d := downloader.NewDownloader()
						for doc := range server.Downloads {
							// total.Inc()

							gifPath := filepath.Join("./tmp/", fmt.Sprintf("%d.%s", doc.ID, doc.Ext))
							fmt.Printf("Got doc: %v, %v\n",
								doc.ID,
								gifPath,
							)

							if _, err := os.Stat(gifPath); err == nil {
								// File exists, skipping.
								//
								// Note that we are not completely sure that existing
								// file is exactly same as this gif (e.g. partial
								// download), so not removing even with --rm flag.
								continue
							}

							// Downloading gif to gifPath.
							// loc := doc.AsInputDocumentFileLocation()
							if _, err := d.Download(api, doc.FileLoc).ToPath(ctx, gifPath); err != nil {
								fmt.Printf("Download doc err: %v, %v, %v\n", err,
									doc.ID,
									gifPath)
								continue
							}
							// downloaded.Inc()
							fmt.Printf("Download doc ok: %v, %v\n",
								doc.ID,
								gifPath,
							)
						}
						return nil
					})
				}

				if err := g.Wait(); err != nil {
					fmt.Printf("err %v\n", err)
					return
				}
				fmt.Printf("Finished OK\n")
			}()
			// Waiting until context is done.
			fmt.Println("Listening for updates. Interrupt (Ctrl+C) to stop.")
			return updatesRecovery.Run(ctx, api, self.ID, updates.AuthOptions{
				IsBot: self.Bot,
				OnStart: func(ctx context.Context) {
					fmt.Println("Update recovery initialized and started, listening for events")
				},
			})
		}); err != nil {
			return errors.Wrap(err, "run")
		}
		return nil
	}

	fmt.Println("start running...")
	runner(ctx)
	// return waiter.Run(ctx, runner)
	log.Println("end running?")
	return nil
}

type DownloadInfo struct {
	ID      int64
	Ext     string
	FileLoc tg.InputFileLocationClass
}

type Server struct {
	DBPeer    *pebble.PeerStorage
	Downloads chan DownloadInfo
}

func (s *Server) onMessage(ctx context.Context, e tg.Entities, msg *tg.Message) error {
	if msg.Media != nil {
		fmt.Printf("media type: %v\n", msg.Media.TypeName())
		switch msg.Media.TypeName() {
		case "messageMediaEmpty": // messageMediaEmpty#3ded6320
			_, ok := msg.Media.(*tg.MessageMediaEmpty)
			if !ok {
				fmt.Printf("unknown media: %v\n", msg.Media)
				return nil
			}
		case "messageMediaPhoto": // messageMediaPhoto#695150d7
			media, ok := msg.Media.(*tg.MessageMediaPhoto)
			if !ok {
				fmt.Printf("unknown media: %v\n", msg.Media)
				return nil
			}
			photo, ok := media.Photo.AsNotEmpty()
			if !ok {
				fmt.Printf("unknown Photo: %v\n", media.Photo)
				return nil
			}

			fmt.Printf("Photo: %v\n", photo)
			var thumbSize string
			for _, thmb := range photo.Sizes {
				thb, ok := thmb.AsNotEmpty()
				if ok {
					thumbSize = thb.GetType()
					if len(thumbSize) > 0 {
						break
					}
				}
			}
			if len(thumbSize) > 0 {
				s.Downloads <- DownloadInfo{
					ID: photo.ID,
					// Ext: photo.,
					Ext: "JPEG",
					FileLoc: &tg.InputPhotoFileLocation{
						ID:            photo.ID,
						AccessHash:    photo.AccessHash,
						FileReference: photo.FileReference,
						ThumbSize:     thumbSize,
					},
				}
			}

		case "messageMediaGeo": // messageMediaGeo#56e0d474
			media, ok := msg.Media.(*tg.MessageMediaGeo)
			if !ok {
				fmt.Printf("unknown media: %v\n", msg.Media)
				return nil
			}
			geo, ok := media.Geo.AsNotEmpty()
			if !ok {
				fmt.Printf("unknown Geo: %v\n", media.Geo)
				return nil
			}
			fmt.Printf("Geo: %v\n", geo)
		case "messageMediaContact": // messageMediaContact#70322949
			media, ok := msg.Media.(*tg.MessageMediaContact)
			if !ok {
				fmt.Printf("unknown media: %v\n", msg.Media)
				return nil
			}
			fmt.Printf("media: %v\n", media)
		case "messageMediaUnsupported": // messageMediaUnsupported#9f84f49e
			media, ok := msg.Media.(*tg.MessageMediaUnsupported)
			if !ok {
				fmt.Printf("unknown media: %v\n", msg.Media)
				return nil
			}
			fmt.Printf("MessageMediaUnsupported media: %v\n", media)
		case "messageMediaDocument": // messageMediaDocument#dd570bd5
			media, ok := msg.Media.(*tg.MessageMediaDocument)
			if !ok {
				fmt.Printf("unknown media: %v\n", msg.Media)
				return nil
			}
			document, ok := media.Document.AsNotEmpty()
			if !ok {
				fmt.Printf("unknown Document: %v\n", media.Document)
				return nil
			}

			fmt.Printf("Document: %v\n", document)

			s.Downloads <- DownloadInfo{
				ID: document.ID,
				// Ext: photo.,
				Ext:     strings.ReplaceAll(document.MimeType, "/", "."),
				FileLoc: document.AsInputDocumentFileLocation(),
			}
		case "messageMediaWebPage": // messageMediaWebPage#ddf10c3b
			media, ok := msg.Media.(*tg.MessageMediaWebPage)
			if !ok {
				fmt.Printf("unknown media: %v\n", msg.Media)
				return nil
			}
			webpage, ok := media.Webpage.AsModified()
			if !ok {
				fmt.Printf("unknown Webpage: %v\n", media.Webpage)
				return nil
			}
			fmt.Printf("Webpage: %v\n", webpage)
		case "messageMediaVenue": // messageMediaVenue#2ec0533f
			media, ok := msg.Media.(*tg.MessageMediaVenue)
			if !ok {
				fmt.Printf("unknown media: %v\n", msg.Media)
				return nil
			}
			fmt.Printf("MessageMediaVenue: %v\n", media)
		case "messageMediaGame": // messageMediaGame#fdb19008
			media, ok := msg.Media.(*tg.MessageMediaGame)
			if !ok {
				fmt.Printf("unknown media: %v\n", msg.Media)
				return nil
			}
			fmt.Printf("Game: %v\n", media.Game)
		case "messageMediaInvoice": // messageMediaInvoice#f6a548d3
			media, ok := msg.Media.(*tg.MessageMediaInvoice)
			if !ok {
				fmt.Printf("unknown media: %v\n", msg.Media)
				return nil
			}
			fmt.Printf("MessageMediaInvoice: %v\n", media)
		case "messageMediaGeoLive": // messageMediaGeoLive#b940c666
			media, ok := msg.Media.(*tg.MessageMediaGeoLive)
			if !ok {
				fmt.Printf("unknown media: %v\n", msg.Media)
				return nil
			}
			fmt.Printf("MessageMediaGeoLive: %v\n", media)
		case "messageMediaPoll": // messageMediaPoll#4bd6e798
			media, ok := msg.Media.(*tg.MessageMediaPoll)
			if !ok {
				fmt.Printf("unknown media: %v\n", msg.Media)
				return nil
			}
			fmt.Printf("MessageMediaPoll: %v\n", media)
		case "messageMediaDice": // messageMediaDice#3f7ee58b
			media, ok := msg.Media.(*tg.MessageMediaDice)
			if !ok {
				fmt.Printf("unknown media: %v\n", msg.Media)
				return nil
			}
			fmt.Printf("MessageMediaDice: %v\n", media)
		case "messageMediaStory": // messageMediaStory#68cb6283
			media, ok := msg.Media.(*tg.MessageMediaStory)
			if !ok {
				fmt.Printf("unknown media: %v\n", msg.Media)
				return nil
			}
			fmt.Printf("MessageMediaStory: %v\n", media)
		case "messageMediaGiveaway": // messageMediaGiveaway#aa073beb
			media, ok := msg.Media.(*tg.MessageMediaGiveaway)
			if !ok {
				fmt.Printf("unknown media: %v\n", msg.Media)
				return nil
			}
			fmt.Printf("MessageMediaGiveaway: %v\n", media)
		case "messageMediaGiveawayResults": // messageMediaGiveawayResults#ceaa3ea1
			media, ok := msg.Media.(*tg.MessageMediaGiveawayResults)
			if !ok {
				fmt.Printf("unknown media: %v\n", msg.Media)
				return nil
			}
			fmt.Printf("MessageMediaGiveawayResults: %v\n", media)
		case "messageMediaPaidMedia": // messageMediaPaidMedia#a8852491
			media, ok := msg.Media.(*tg.MessageMediaPaidMedia)
			if !ok {
				fmt.Printf("unknown media: %v\n", msg.Media)
				return nil
			}
			fmt.Printf("MessageMediaPaidMedia: %v\n", media)
		default:
		}
	}

	if msg.Out {
		// Outgoing message.
		fmt.Printf("Outgoing: %v\n", msg)
		return nil
	}

	// Use PeerID to find peer because *Short updates does not contain any entities, so it necessary to
	// store some entities.
	//
	// Storage can be filled using PeerCollector (i.e. fetching all dialogs first).
	p, err := storage.FindPeer(ctx, s.DBPeer, msg.GetPeerID())
	if err != nil {
		fmt.Printf("FindPeer: %v, %v\n", msg, err)
		return err
	}

	fmt.Printf("%s: %v\n", p, msg)
	return nil
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

// SetDefaults sets default values.
func getDevice() (c telegram.DeviceConfig) {
	const notAvailable = "n/a"

	// Strings must be non-empty, so set notAvailable if default value is empty.
	set := func(to *string, value string) {
		if value != "" {
			*to = value
		} else {
			*to = notAvailable
		}
	}

	set(&c.SystemVersion, runtime.GOOS)
	c.AppVersion = "PEI's Telegram v0.3.2"
	c.DeviceModel = "pei's mtproto device"
	c.SystemLangCode = "en"
	c.LangCode = "en"

	return
}

// Prod returns production DC list.
// dcs.Prod()
func DCProd() dcs.List {
	// https://github.com/telegramdesktop/tdesktop/blob/dev/Telegram/SourceFiles/mtproto/mtproto_dc_options.cpp
	// Also available with client.API().HelpGetConfig(ctx) [tg.DCOption].
	// TODO(ernado): automate update from HelpGetConfig.
	/*
		{ 1, "149.154.175.50" , 443 },
		{ 2, "149.154.167.51" , 443 },
		{ 2, "95.161.76.100"  , 443 },
		{ 3, "149.154.175.100", 443 },
		{ 4, "149.154.167.91" , 443 },
		{ 5, "149.154.171.5"  , 443 },
	*/
	return dcs.List{
		Options: []tg.DCOption{
			{ID: 2, IPAddress: "149.154.167.50", Port: 443},
			{ID: 1, IPAddress: "149.154.175.50", Port: 443},
			{ID: 2, IPAddress: "149.154.167.51", Port: 443},
			{ID: 2, IPAddress: "95.161.76.100", Port: 443},
			{ID: 5, IPAddress: "149.154.171.5", Port: 443},
			{
				ID:        1,
				IPAddress: "149.154.175.52",
				Port:      443,
			},
			{
				Static:    true,
				ID:        1,
				IPAddress: "149.154.175.53",
				Port:      443,
			},
			{
				Ipv6:      true,
				ID:        1,
				IPAddress: "2001:0b28:f23d:f001:0000:0000:0000:000a",
				Port:      443,
			},
			{
				ID:        2,
				IPAddress: "149.154.167.41",
				Port:      443,
			},
			{
				Static:    true,
				ID:        2,
				IPAddress: "149.154.167.41",
				Port:      443,
			},
			{
				MediaOnly: true,
				ID:        2,
				IPAddress: "149.154.167.222",
				Port:      443,
			},
			{
				Ipv6:      true,
				ID:        2,
				IPAddress: "2001:067c:04e8:f002:0000:0000:0000:000a",
				Port:      443,
			},
			{
				Ipv6:      true,
				MediaOnly: true,
				ID:        2,
				IPAddress: "2001:067c:04e8:f002:0000:0000:0000:000b",
				Port:      443,
			},
			{
				ID:        3,
				IPAddress: "149.154.175.100",
				Port:      443,
			},
			{
				Static:    true,
				ID:        3,
				IPAddress: "149.154.175.100",
				Port:      443,
			},
			{
				Ipv6:      true,
				ID:        3,
				IPAddress: "2001:0b28:f23d:f003:0000:0000:0000:000a",
				Port:      443,
			},
			{
				ID:        4,
				IPAddress: "149.154.167.91",
				Port:      443,
			},
			{
				Static:    true,
				ID:        4,
				IPAddress: "149.154.167.91",
				Port:      443,
			},
			{
				Ipv6:      true,
				ID:        4,
				IPAddress: "2001:067c:04e8:f004:0000:0000:0000:000a",
				Port:      443,
			},
			{
				MediaOnly: true,
				ID:        4,
				IPAddress: "149.154.166.120",
				Port:      443,
			},
			{
				Ipv6:      true,
				MediaOnly: true,
				ID:        4,
				IPAddress: "2001:067c:04e8:f004:0000:0000:0000:000b",
				Port:      443,
			},
			{
				Ipv6:      true,
				ID:        5,
				IPAddress: "2001:0b28:f23f:f005:0000:0000:0000:000a",
				Port:      443,
			},
			{
				ID:        5,
				IPAddress: "91.108.56.191",
				Port:      443,
			},
			{
				Static:    true,
				ID:        5,
				IPAddress: "91.108.56.191",
				Port:      443,
			},
		},
		Domains: map[int]string{
			1: "wss://pluto.web.telegram.org/apiws",
			2: "wss://venus.web.telegram.org/apiws",
			3: "wss://aurora.web.telegram.org/apiws",
			4: "wss://vesta.web.telegram.org/apiws",
			5: "wss://flora.web.telegram.org/apiws",
		},
	}
}
