package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
)

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", glowbomHealthHandler)
	mux.HandleFunc("/", glowbomBackendHomeHandler)
	mux.HandleFunc("/favicon.png", glowbomFaviconHandler)
	mux.HandleFunc("/logo-svg.svg", glowbomLogoSVGHandler)

	mux.HandleFunc("/chatWithAI", chatWithAIHandler)
	mux.HandleFunc("/webSearch", webSearchHandler)
	mux.HandleFunc("/analyzeVideo", analyzeVideoHandler)

	mux.HandleFunc("/image", generateImageHandler)
	mux.HandleFunc("/audio", generateAudioHandler)
	mux.HandleFunc("/audio/voices", listElevenLabsVoicesHandler)

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
	mux.HandleFunc("/opencode/project", openCodeGetProjectHandler)
	mux.HandleFunc("/opencode/project/history", openCodeProjectHistoryHandler)
	mux.HandleFunc("/opencode/project/pick", openCodePickProjectFolderHandler)
	mux.HandleFunc("/opencode/instructions/files/pick", openCodePickInstructionFilesHandler)
	mux.HandleFunc("/opencode/project/ide/status", openCodeProjectIDEStatusHandler)
	mux.HandleFunc("/opencode/project/open", openCodeProjectOpenHandler)
	mux.HandleFunc("/opencode/project/rename", openCodeRenameProjectHandler)
	mux.HandleFunc("/opencode/project/settings", openCodeUpdateProjectSettingsHandler)
	mux.HandleFunc("/opencode/project/icon", openCodeProjectIconHandler)
	mux.HandleFunc("/opencode/project/icon/generate", openCodeGenerateIconHandler)
	mux.HandleFunc("/opencode/models/available", openCodeAvailableModelsHandler)
	mux.HandleFunc("/providers/openai/models", openAIModelsHandler)
	mux.HandleFunc("/opencode/refine", openCodeRefineHandler)
	mux.HandleFunc("/opencode/media/postpass", openCodeMediaPostPassHandler)
	mux.HandleFunc("/opencode/media/approval/respond", openCodeMediaApprovalRespondHandler)
	mux.HandleFunc("/opencode/verify", openCodeVerifyHandler)
	mux.HandleFunc("/opencode/question/respond", openCodeQuestionRespondHandler)
	mux.HandleFunc("/opencode/permission/respond", openCodePermissionRespondHandler)

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
	log.Fatal(http.ListenAndServe(listenAddr, withGlowbomSecurity(mux)))
}
