package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if err := validateBackendSecurity(); err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	if os.Getenv("GLOWBOM_DESKTOP") == "1" {
		mux.HandleFunc("/desktop/ready", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Glowbom-Instance", glowbomServerToken())
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusNoContent)
		})
	}
	uploads, err := os.MkdirTemp("", "glowbom-attachments-")
	if err != nil {
		log.Fatal("Could not prepare attachment storage")
	}
	defer os.RemoveAll(uploads)
	chatDirectory, err := os.MkdirTemp("", "glowbom-chat-")
	if err != nil {
		log.Fatal("Could not prepare chat workspace")
	}
	defer os.RemoveAll(chatDirectory)
	chat := newChatService(chatDirectory, uploads)
	defer glowbomCodex.close()
	companion := newCompanionManager(mux)
	companion.restoreRecentRuns()
	defer companion.Shutdown()
	mux.Handle("/companion", companion)
	mux.Handle("/companion/responded", companion)
	mux.Handle("/companion/respond", companion)
	mux.Handle("/companion/build-model", companion)
	mux.Handle("/companion/cancel", companion)
	mux.Handle("/companion/pairing/", companion)
	mux.HandleFunc("/chat/models", chat.modelsHandler)
	mux.HandleFunc("/settings/build/jev", jevStatusHandler())
	mux.HandleFunc("/settings/apple-intelligence", appleIntelligenceSettingsHandler())
	mux.HandleFunc("/settings/dictation", dictationSettingsHandler)
	mux.HandleFunc("/audio/transcribe", dictationTranscribeHandler)
	mux.HandleFunc("/settings/local-ai", newLocalAIService(chat).handler)
	mux.HandleFunc("/settings/providers/connect", providerConnectionHandler(chat))
	mux.HandleFunc("/settings/providers/status", providerStatusHandler(chat))
	mux.HandleFunc("/settings/providers/refresh", providerRefreshHandler(chat))
	mux.HandleFunc("/settings/agents", agentSetupHandler())
	mux.HandleFunc("/settings/agents/install", agentSetupInstallHandler())
	mux.HandleFunc("/settings/acp", acpSettingsHandler())
	mux.HandleFunc("/settings/acp/test", acpTestHandler())
	for _, route := range []string{"/codex/status", "/codex/restart", "/codex/login", "/codex/login/cancel"} {
		mux.HandleFunc(route, codexRuntimeHandler)
	}
	mux.HandleFunc("/chat/stream", chat.streamHandler)
	mux.HandleFunc("/chat/project-name", chat.projectNameHandler)
	mux.HandleFunc("/chat/history", chat.historyHandler)
	mux.HandleFunc("/chat/result", chat.resultHandler)
	mux.HandleFunc("/opencode/instructions/upload", instructionUploadHandler(uploads))
	previews := newProjectPreviewManager()
	companion.previews = previews
	defer previews.Close()
	mux.Handle("/preview", previews)

	mux.HandleFunc("/healthz", glowbomHealthHandler)
	mux.HandleFunc("/", glowbomBackendHomeHandler)
	mux.HandleFunc("/favicon.png", glowbomFaviconHandler)
	mux.HandleFunc("/logo-svg.svg", glowbomLogoSVGHandler)
	account := newAccountBridge(runAccountCLI)
	glowbomImageAccount = account
	for _, route := range []string{"/account/status", "/account/login", "/account/login/cancel", "/account/logout", "/account/project"} {
		mux.Handle(route, account)
	}

	mux.HandleFunc("/chatWithAI", chatWithAIHandler)
	mux.HandleFunc("/webSearch", webSearchHandler)
	mux.HandleFunc("/analyzeVideo", analyzeVideoHandler)

	mux.HandleFunc("/image", generateImageHandler)
	mux.HandleFunc("/studio/assets", studioAssetsHandler)
	mux.HandleFunc("/studio/projects", studioProjectsHandler)
	mux.HandleFunc("/studio/audio", studioAudioHandler)
	mux.HandleFunc("/studio/audio/content", studioAudioContentHandler)
	mux.HandleFunc("/studio/audio/generate", studioAudioGenerateHandler)
	mux.HandleFunc("/studio/audio/use", studioAudioUseHandler)
	mux.HandleFunc("/studio/images/use", studioImageUseHandler)
	mux.HandleFunc("/studio/assets/info", studioAssetInfoHandler)
	mux.HandleFunc("/studio/images", studioImagesHandler)
	mux.HandleFunc("/studio/images/status", studioImagesStatusHandler)
	mux.HandleFunc("/studio/images/content", studioImageContentHandler)
	mux.HandleFunc("/studio/images/generate", studioImageGenerateHandler)
	mux.HandleFunc("/studio/images/capabilities", studioImageCapabilitiesHandler)
	mux.HandleFunc("/studio/providers/key", studioProviderKeyHandler)
	mux.HandleFunc("/studio/generation/status", studioGenerationStatusHandler)
	mux.HandleFunc("/studio/generation/cancel", studioGenerationCancelHandler)
	mux.HandleFunc("/studio/generation/resume", studioGenerationResumeHandler)
	mux.HandleFunc("/studio/videos", studioVideosHandler)
	mux.HandleFunc("/studio/videos/content", studioVideoContentHandler)
	mux.HandleFunc("/studio/videos/generate", studioVideoGenerateHandler)
	mux.HandleFunc("/studio/videos/capabilities", studioVideoCapabilitiesHandler)
	mux.HandleFunc("/studio/videos/key", studioVideoKeyHandler)
	mux.HandleFunc("/studio/clips/capabilities", studioClipCapabilitiesHandler)
	mux.HandleFunc("/studio/clips/setup", studioClipSetupHandler)
	mux.HandleFunc("/studio/clips/probe", studioClipProbeHandler)
	mux.HandleFunc("/studio/clips/render", studioClipRenderHandler)
	mux.HandleFunc("/studio/clips/status", studioClipStatusHandler)
	mux.HandleFunc("/studio/clips/cancel", studioClipCancelHandler)
	mux.HandleFunc("/studio/clips/playback", studioClipPlaybackHandler)
	mux.HandleFunc("/studio/clips/source", studioClipSourceHandler)
	defer studioClips.Close()
	mux.HandleFunc("/audio", generateAudioHandler)
	mux.HandleFunc("/audio/voices", listElevenLabsVoicesHandler)
	mux.HandleFunc("/audio/local", localVoiceSpeechHandler(localVoiceHTTPClient))
	mux.HandleFunc("/audio/local/voices", localVoiceVoicesHandler(localVoiceHTTPClient))
	mux.HandleFunc("/audio/key", voiceKeyHandler(systemVoiceKeyStore{}))
	mux.Handle("/live/app", newLiveAppHandler())
	mux.Handle("/buzz/members", newBuzzMembersHandler(runBuzzRead))
	buzzSession := newBuzzSession(runBuzzRead)
	mux.Handle("/buzz/session", buzzSession)
	mux.Handle("/buzz/session/refresh", buzzSession)
	mux.Handle("/buzz/session/avatar", buzzSession)
	mux.Handle("/buzz/session/messages", buzzSession)
	mux.Handle("/buzz/session/speech", buzzSession)
	mux.Handle("/buzz/session/profiles", buzzSession)
	mux.Handle("/buzz/session/voices", buzzSession)
	mux.Handle("/buzz/session/voice-preview", buzzSession)

	// Veo video generation endpoints
	mux.HandleFunc("/generateVeoVideo", generateVeoVideoHandler)
	mux.HandleFunc("/pollVeoOperation", pollVeoOperationHandler)

	// OpenCode agent endpoints
	mux.HandleFunc("/opencode/translate", openCodeTranslateHandler)
	mux.HandleFunc("/opencode/health", openCodeHealthHandler)
	mux.HandleFunc("/opencode/about", openCodeAboutHandler)
	mux.HandleFunc("/opencode/about/project-description", openCodeProjectDescriptionHandler)
	mux.HandleFunc("/opencode/auth/status", openCodeAuthStatusHandler)
	mux.HandleFunc("/opencode/auth/openai/oauth/start", openCodeOpenAIOAuthStartHandler)
	mux.HandleFunc("/opencode/auth/openai/oauth/status", openCodeOpenAIOAuthStatusHandler)
	mux.HandleFunc("/opencode/auth/openai/oauth/callback", openCodeOpenAIOAuthCallbackHandler)
	mux.HandleFunc("/opencode/auth/openai/connect", openCodeOpenAIConnectHandler)
	mux.HandleFunc("/opencode/auth/openai/disconnect", openCodeOpenAIDisconnectHandler)
	mux.HandleFunc("/opencode/project/init", openCodeInitProjectHandler)
	mux.HandleFunc("/opencode/project/create", openCodeCreateProjectHandler)
	mux.HandleFunc("/opencode/project/check", openCodeCheckProjectHandler)
	mux.HandleFunc("/opencode/project", openCodeGetProjectHandler)
	mux.HandleFunc("/opencode/project/history", openCodeProjectHistoryHandler)
	mux.HandleFunc("/project-book", projectBookHandler)
	mux.HandleFunc("/project-book/visual", projectBookVisualHandler)
	mux.HandleFunc("/project-book/story", projectBookStoryHandler)
	mux.HandleFunc("/project-book/models", projectBookModelsHandler)
	mux.HandleFunc("/settings/project-book", projectBookWritingSettingsHandler)
	mux.HandleFunc("/project-book/media", projectBookMediaHandler)
	mux.HandleFunc("/opencode/project/pick", openCodePickProjectFolderHandler)
	mux.HandleFunc("/opencode/instructions/files/pick", openCodePickInstructionFilesHandler)
	mux.HandleFunc("/opencode/project/ide/status", openCodeProjectIDEStatusHandler)
	mux.HandleFunc("/opencode/project/open", openCodeProjectOpenHandler)
	mux.HandleFunc("/opencode/project/rename", openCodeRenameProjectHandler)
	mux.HandleFunc("/opencode/project/settings", openCodeUpdateProjectSettingsHandler)
	mux.HandleFunc("/opencode/project/icon", openCodeProjectIconHandler)
	mux.HandleFunc("/opencode/project/icon/generate", openCodeGenerateIconHandler)
	mux.HandleFunc("/opencode/project/icon/sources", openCodeIconSourcesHandler)
	mux.HandleFunc("/opencode/models/available", openCodeAvailableModelsHandler)
	mux.HandleFunc("/providers/openai/models", openAIModelsHandler)
	mux.HandleFunc("/opencode/refine", companion.guardBuild(openCodeRefineHandler))
	mux.HandleFunc("/opencode/steer", openCodeSteerHandler)
	mux.HandleFunc("/opencode/media/postpass", openCodeMediaPostPassHandler)
	mux.HandleFunc("/opencode/media/approval/respond", companion.guardResponse("media", openCodeMediaApprovalRespondHandler))
	mux.HandleFunc("/opencode/verify", openCodeVerifyHandler)
	mux.HandleFunc("/opencode/question/respond", companion.guardResponse("question", openCodeQuestionRespondHandler))
	mux.HandleFunc("/opencode/permission/respond", companion.guardResponse("permission", openCodePermissionRespondHandler))

	port := os.Getenv("GLOWBOM_PORT")
	if port == "" {
		port = "4569"
	}
	listenAddr := backendListenAddr(port)
	if glowbomServerToken() == "" {
		fmt.Println("Warning: GLOWBOM_SERVER_TOKEN is not set; backend auth is disabled.")
	} else {
		fmt.Println("Backend auth enabled for non-public routes.")
	}
	fmt.Printf("Server running on http://%s\n", listenAddr)
	handler, err := desktopHandler(mux)
	if err != nil {
		log.Fatal(err)
	}
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Addr: listenAddr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server.BaseContext = func(net.Listener) context.Context { return ctx }
	if os.Getenv("GLOWBOM_DESKTOP") == "1" {
		defer stopDesktopProcessGroup()
		go func() { _, _ = io.Copy(io.Discard, os.Stdin); stop() }()
	}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		companion.Shutdown()
		glowbomCodex.close()
		studioClips.Close()
		previews.Close()
		stopAppleIntelligenceBridge()
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		previews.Close()
		log.Printf("Server stopped: %v", err)
	}
	stop()
	<-shutdownDone
}
