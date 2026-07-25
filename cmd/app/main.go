package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"taxi-mvp/internal/accounting"
	"taxi-mvp/internal/bot"
	driverbot "taxi-mvp/internal/bot/driver"
	"taxi-mvp/internal/config"
	"taxi-mvp/internal/db"
	"taxi-mvp/internal/db/broadcasts"
	"taxi-mvp/internal/db/driverapprepair"
	"taxi-mvp/internal/db/driverlogincodes"
	"taxi-mvp/internal/db/ledgerrepair"
	"taxi-mvp/internal/db/legalfingerrepair"
	"taxi-mvp/internal/db/legalrepair"
	"taxi-mvp/internal/db/riderappnotifications"
	"taxi-mvp/internal/db/riderapprepair"
	"taxi-mvp/internal/db/riderlogincodes"
	"taxi-mvp/internal/repositories"
	"taxi-mvp/internal/safe"
	"taxi-mvp/internal/server"
	"taxi-mvp/internal/services"
	"taxi-mvp/internal/ws"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	database, err := db.Open(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	defer db.Close(database)
	if err := legalrepair.Ensure(context.Background(), database); err != nil {
		log.Fatalf("legal schema repair: %v", err)
	}
	if err := ledgerrepair.Ensure(context.Background(), database); err != nil {
		log.Fatalf("driver_ledger schema repair: %v", err)
	}
	if err := legalfingerrepair.Ensure(context.Background(), database); err != nil {
		log.Fatalf("drivers legal fingerprint column repair: %v", err)
	}
	if err := driverapprepair.Ensure(context.Background(), database); err != nil {
		log.Fatalf("drivers app location columns repair: %v", err)
	}
	if err := riderapprepair.Ensure(context.Background(), database); err != nil {
		log.Fatalf("users rider app last-seen column repair: %v", err)
	}
	if err := driverlogincodes.Ensure(context.Background(), database); err != nil {
		log.Fatalf("driver login codes schema: %v", err)
	}
	if err := riderlogincodes.Ensure(context.Background(), database); err != nil {
		log.Fatalf("rider login codes schema: %v", err)
	}
	if err := riderappnotifications.Ensure(context.Background(), database); err != nil {
		log.Fatalf("rider app notifications schema: %v", err)
	}
	if err := broadcasts.Ensure(context.Background(), database); err != nil {
		log.Fatalf("broadcasts schema: %v", err)
	}
	if err := accounting.BackfillMissingSignupPromos(context.Background(), database); err != nil {
		log.Printf("accounting: signup promo backfill: %v", err)
	}

	riderBot, err := tgbotapi.NewBotAPI(cfg.RiderBotToken)
	if err != nil {
		log.Fatalf("rider bot: %v", err)
	}
	driverBot, err := tgbotapi.NewBotAPI(cfg.DriverBotToken)
	if err != nil {
		log.Fatalf("driver bot: %v", err)
	}
	var adminBot *tgbotapi.BotAPI
	if cfg.AdminBotToken != "" {
		adminBot, err = tgbotapi.NewBotAPI(cfg.AdminBotToken)
		if err != nil {
			log.Printf("admin bot: %v (continuing without admin bot)", err)
			adminBot = nil
		}
	}

	matchSvc := services.NewMatchService(database, driverBot, cfg)
	assignSvc := services.NewAssignmentService(database, riderBot, driverBot, cfg)
	hub := ws.NewHub()
	// The hub owns all WebSocket fan-out; losing it silently would stop every
	// live trip update, so supervise it rather than just recovering.
	hubCtx, hubCancel := context.WithCancel(context.Background())
	defer hubCancel()
	safe.GoSupervised(hubCtx, "ws_hub", hub.Run)
	tripRepo := repositories.NewTripRepo(database)
	paymentRepo := repositories.NewPaymentRepository(database)
	fareSvc := services.NewFareService(database, cfg)
	tripSvc := services.NewTripService(database, tripRepo, riderBot, driverBot, cfg, hub, fareSvc, paymentRepo)

	// Native rider auth (Flutter rider app: phone + Telegram OTP).
	riderLoginCodesRepo := repositories.NewRiderLoginCodesRepo(database)
	riderAuthSessionsRepo := repositories.NewRiderAuthSessionsRepo(database)
	riderAuthTokens := services.NewRiderAuthTokenService(riderAuthSessionsRepo, cfg.RiderBotToken)
	riderAuthSvc := services.NewRiderAuthService(database, riderLoginCodesRepo, riderAuthTokens, riderBot, services.RiderAuthConfig{})
	riderReqAppSvc := services.NewRiderRequestAppService(database, cfg, matchSvc)
	tripSvc.OnDriverStatusUpdate = func(telegramID int64) {
		driverbot.UpdatePinnedStatusForChat(driverBot, database, cfg, telegramID)
	}
	// A driver cancellation returns the ride to the pool; re-dispatch it immediately.
	tripSvc.OnRequestRequeued = func(requestID string) {
		matchSvc.StartPriorityDispatch(context.Background(), requestID)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Println("shutdown signal received")
		cancel()
	}()

	// Supervised: a panic in any of these would otherwise kill the whole process
	// (single instance — see render.yaml), and plain recovery would leave the
	// worker silently dead, so requests would stop expiring or offers would stop
	// being dispatched with nothing to show why.
	safe.GoSupervised(ctx, "expiry_worker", func() { assignSvc.RunExpiryWorker(ctx) })
	safe.GoSupervised(ctx, "radius_expansion_worker", func() { assignSvc.RunRadiusExpansionWorker(ctx, matchSvc) })
	safe.GoSupervised(ctx, "driver_approval_notifier", func() { services.RunDriverApprovalNotifier(ctx, database, driverBot) })
	safe.GoSupervised(ctx, "driver_app_auto_offline_worker", func() { services.RunDriverAppAutoOfflineWorker(ctx, database) })
	safe.GoSupervised(ctx, "legal_reaccept_notifier", func() { driverbot.RunLegalReacceptNotifier(ctx, database, driverBot) })
	safe.GoSupervised(ctx, "broadcast_fanout_worker", func() { services.RunBroadcastFanoutWorker(ctx, database, riderBot, cfg) })
	safe.GoSupervised(ctx, "retention_worker", func() { services.RunRetentionWorker(ctx, database) })

	srv := server.New(database, cfg, tripSvc, matchSvc, assignSvc, driverBot, riderBot, hub, fareSvc, riderAuthSvc, riderReqAppSvc)
	httpServer := &http.Server{Addr: cfg.APIAddr, Handler: srv}
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("api server: %v", err)
		}
	}()

	// Delay bot startup so a previous process (e.g. old deploy) can release getUpdates and avoid "Conflict: terminated by other getUpdates request".
	const botStartDelay = 8 * time.Second
	select {
	case <-time.After(botStartDelay):
		log.Printf("bot startup delay done, starting Telegram bots")
	case <-ctx.Done():
		log.Println("shutdown during bot delay")
		shutdownHTTPServer(httpServer)
		return
	}

	// Bots are deliberately NOT restarted in-process. Telegram allows one
	// getUpdates consumer per token, and tgbotapi's GetUpdatesChan goroutine is
	// stopped only by StopReceivingUpdates — re-entering the bot would leave the
	// old poller alive, so Telegram would answer one of them with 409 while the
	// other kept draining updates into an abandoned channel and losing them.
	// Instead a bot that panics or returns unexpectedly cancels the root context,
	// the process exits cleanly, and the platform restarts it with one poller.
	// This also makes a silently dead bot loud, rather than leaving the HTTP API
	// serving happily while nobody is reading Telegram.
	var wg sync.WaitGroup
	runBot := func(name string, fn func()) {
		defer wg.Done()
		if safe.Run(name, fn) {
			log.Printf("%s panicked; shutting down so it restarts with a single Telegram poller", name)
		} else if ctx.Err() == nil {
			log.Printf("%s exited unexpectedly; shutting down so it restarts", name)
		} else {
			return // normal shutdown
		}
		cancel()
	}

	wg.Add(2)
	go runBot("rider_bot", func() {
		bot.RunRiderBot(ctx, cfg, database, riderBot, matchSvc, tripSvc)
	})
	go runBot("driver_bot", func() {
		bot.RunDriverBot(ctx, cfg, database, driverBot, matchSvc, assignSvc, tripSvc)
	})
	if adminBot != nil && cfg.AdminID != 0 {
		wg.Add(1)
		go runBot("admin_bot", func() {
			bot.RunAdminBot(ctx, cfg, database, adminBot, fareSvc, driverBot)
		})
	}

	wg.Wait()
	shutdownHTTPServer(httpServer)
	log.Println("graceful shutdown complete")
}

// shutdownHTTPServer drains the API server with a hard deadline so exit cannot hang forever.
func shutdownHTTPServer(s *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		log.Printf("api server shutdown: %v", err)
	}
}
