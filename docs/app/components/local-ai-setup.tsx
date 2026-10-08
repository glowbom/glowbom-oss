import { useState } from "react";

type Setup = "ready" | "need-setup" | "unsure";
type Account = "chatgpt" | "xai-api" | "grok" | "none";
type Location = "local" | "hosted" | "both";
type Computer = "apple-silicon" | "intel-mac" | "linux" | "windows" | "unsure";
type FirstTask = "chat-build" | "images" | "voice";

type Answers = {
  setup?: Setup;
  accounts: Account[];
  location?: Location;
  computer?: Computer;
  firstTask?: FirstTask;
};

const startingAnswers: Answers = { accounts: [] };

const choices = {
  setup: [
    { value: "ready", label: "Yes, both are ready" },
    { value: "need-setup", label: "I need to install them" },
    { value: "unsure", label: "I'm not sure" },
  ],
  accounts: [
    { value: "chatgpt", label: "ChatGPT Plus or Pro" },
    { value: "xai-api", label: "An xAI API key" },
    { value: "grok", label: "A Grok subscription" },
    { value: "none", label: "None or I'm not sure" },
  ],
  location: [
    { value: "local", label: "On my computer" },
    { value: "hosted", label: "Through an online provider" },
    { value: "both", label: "I'd like to try both" },
  ],
  computer: [
    { value: "apple-silicon", label: "Mac with an M-series chip" },
    { value: "intel-mac", label: "Intel Mac" },
    { value: "linux", label: "Linux computer" },
    { value: "windows", label: "Windows computer" },
    { value: "unsure", label: "I'm not sure" },
  ],
  firstTask: [
    { value: "chat-build", label: "Talk through an idea and build" },
    { value: "images", label: "Create images" },
    { value: "voice", label: "Try voice" },
  ],
} as const;

const prompts = [
  "Do you have Glowbom OSS and OpenCode installed?",
  "Which accounts or keys do you already have?",
  "Where would you like AI to run?",
  "What computer will you use?",
  "What would you like to try first?",
];

function optionsForStep(step: number) {
  switch (step) {
    case 0: return choices.setup;
    case 1: return choices.accounts;
    case 2: return choices.location;
    case 3: return choices.computer;
    default: return choices.firstTask;
  }
}

function selectedForStep(answers: Answers, step: number): string[] {
  switch (step) {
    case 0: return answers.setup ? [answers.setup] : [];
    case 1: return answers.accounts;
    case 2: return answers.location ? [answers.location] : [];
    case 3: return answers.computer ? [answers.computer] : [];
    default: return answers.firstTask ? [answers.firstTask] : [];
  }
}

function guidance(answers: Answers): string[] {
  const steps: string[] = [];

  if (answers.setup !== "ready") {
    steps.push("Install Glowbom OSS and OpenCode using the Glowbom OSS guide below.");
  }

  if (answers.accounts.includes("chatgpt")) {
    steps.push(answers.firstTask === "chat-build"
      ? "Run opencode auth login and choose the ChatGPT Plus or Pro connection under OpenAI."
      : "For chat and Build later, run opencode auth login and choose the ChatGPT Plus or Pro connection under OpenAI.");
  }
  if (answers.accounts.includes("xai-api")) {
    steps.push("Connect your xAI API key in OpenCode. Check the provider's usage costs before sending a request.");
  }
  if (answers.accounts.includes("grok")) {
    steps.push("Check which Grok features your subscription provides. A Grok subscription does not automatically provide an xAI API key or an OpenCode model connection.");
  }

  if (answers.location !== "hosted") {
    if (answers.firstTask === "chat-build") {
      steps.push(answers.computer === "apple-silicon"
        ? "For a local chat or coding trial, install Ollama and follow the MiMo 9B example below. Set a coding sized context only if your Mac has enough memory."
        : "Check Ollama's setup instructions for your computer. Start with a model your available memory can support, then connect Ollama to OpenCode.");
    } else if (answers.firstTask === "images") {
      steps.push(answers.computer === "apple-silicon"
        ? "For local images, try the Bonsai Image 4B instructions linked below. Save a result, then add the file to your Glowbom project."
        : "Choose a local image tool that supports your computer, then check its model license and setup instructions before downloading.");
    } else {
      steps.push("For local speech, compare the voice tools below. Start with transcription or preset voices, and check the selected engine's license.");
    }
  } else if (answers.accounts.length === 0 || answers.accounts.includes("none")) {
    steps.push("Choose a provider in OpenCode and review its access method and prices before connecting it.");
  }

  if (answers.firstTask === "chat-build") {
    steps.push("Open Glowbom OSS, select a connected model, and try one short chat, Draw, and Build request in a test project. Review and test the generated result.");
  } else if (answers.firstTask === "images") {
    if (answers.location === "hosted") {
      steps.push("Open Glowbom Studio and choose an available image source. Check the source's account access and usage costs before generating.");
    } else if (answers.location === "both") {
      steps.push("Compare a local image with one made through an available Glowbom Studio source. Add the local image file to your project yourself.");
    } else {
      steps.push("Add your locally generated image file to your Glowbom project yourself. Local image models do not appear in Studio's source menu yet.");
    }
  } else {
    steps.push(answers.location === "hosted"
      ? "Start with Glowbom's system voice or a connected ElevenLabs account in Voice settings. Check the provider's costs before playback."
      : "Try a short speech or transcription sample in the separate voice tool, then bring any useful file into your project yourself.");
  }

  steps.push("Read the exact model and tool licenses below before using their results in a business project.");
  return steps;
}

export function LocalAISetup() {
  const [step, setStep] = useState(0);
  const [answers, setAnswers] = useState<Answers>(startingAnswers);
  const [showResults, setShowResults] = useState(false);
  const selected = selectedForStep(answers, step);
  const options = optionsForStep(step);

  function choose(value: string) {
    if (step === 0) setAnswers((current) => ({ ...current, setup: value as Setup }));
    if (step === 2) setAnswers((current) => ({ ...current, location: value as Location }));
    if (step === 3) setAnswers((current) => ({ ...current, computer: value as Computer }));
    if (step === 4) setAnswers((current) => ({ ...current, firstTask: value as FirstTask }));
    if (step === 1) {
      setAnswers((current) => {
        const account = value as Account;
        if (account === "none") return { ...current, accounts: ["none"] };
        const existing = current.accounts.filter((item) => item !== "none");
        const accounts = existing.includes(account)
          ? existing.filter((item) => item !== account)
          : [...existing, account];
        return { ...current, accounts };
      });
    }
  }

  function restart() {
    setAnswers(startingAnswers);
    setStep(0);
    setShowResults(false);
  }

  return (
    <section className="not-prose my-8 rounded-xl border border-fd-border bg-fd-card p-5 text-fd-card-foreground shadow-sm" aria-label="Local AI setup helper">
      <div className="mb-4 flex items-start justify-between gap-4">
        <div>
          <p className="m-0 text-xs font-semibold uppercase tracking-wide text-fd-muted-foreground">Optional setup helper</p>
          <h2 className="mb-1 mt-2 text-xl font-semibold">Find your starting path</h2>
          <p className="m-0 text-sm text-fd-muted-foreground">Five quick answers create a setup checklist. Your answers stay on this page.</p>
        </div>
        {showResults && <button type="button" onClick={restart} className="shrink-0 rounded-md border border-fd-border px-3 py-1.5 text-sm hover:bg-fd-accent">Start again</button>}
      </div>

      {showResults ? (
        <div aria-live="polite">
          <h3 className="mb-3 text-base font-semibold">Your starting checklist</h3>
          <ol className="m-0 list-decimal space-y-2 ps-5 text-sm">
            {guidance(answers).map((item) => <li key={item}>{item}</li>)}
          </ol>
          <div className="mt-4 flex flex-wrap gap-2 text-sm">
            {answers.setup !== "ready" && <a href={`${import.meta.env.BASE_URL}glowbom-oss`} className="rounded-md border border-fd-border px-3 py-2 hover:bg-fd-accent">Glowbom OSS setup</a>}
            {answers.location !== "hosted" && answers.firstTask === "chat-build" && <a href={answers.computer === "apple-silicon" ? "#try-mimo-9b-with-ollama" : "https://docs.ollama.com/quickstart"} className="rounded-md border border-fd-border px-3 py-2 hover:bg-fd-accent">Ollama steps</a>}
            {answers.location === "hosted" && <a href="https://opencode.ai/docs/providers/" className="rounded-md border border-fd-border px-3 py-2 hover:bg-fd-accent">OpenCode providers</a>}
            {(answers.firstTask === "images" || answers.firstTask === "voice") && <a href="#explore-voice-and-image-tools" className="rounded-md border border-fd-border px-3 py-2 hover:bg-fd-accent">Voice and image tools</a>}
          </div>
          <p className="mb-0 mt-4 text-xs text-fd-muted-foreground">This helper does not check accounts, install tools, or save your answers.</p>
        </div>
      ) : (
        <>
          <p className="mb-3 text-xs font-medium text-fd-muted-foreground">Question {step + 1} of {prompts.length}</p>
          <fieldset>
            <legend className="mb-3 text-base font-semibold">{prompts[step]}</legend>
            <div className="grid gap-2 sm:grid-cols-2">
              {options.map((option) => (
                <label key={option.value} className="flex cursor-pointer items-center gap-2 rounded-lg border border-fd-border px-3 py-3 text-sm hover:bg-fd-accent">
                  <input
                    type={step === 1 ? "checkbox" : "radio"}
                    name={`local-ai-question-${step}`}
                    value={option.value}
                    checked={selected.includes(option.value)}
                    onChange={() => choose(option.value)}
                  />
                  <span>{option.label}</span>
                </label>
              ))}
            </div>
          </fieldset>
          <div className="mt-5 flex items-center justify-between gap-3">
            <button type="button" onClick={() => setStep((current) => current - 1)} disabled={step === 0} className="rounded-md border border-fd-border px-3 py-2 text-sm disabled:cursor-not-allowed disabled:opacity-40 hover:bg-fd-accent">Back</button>
            <button
              type="button"
              disabled={selected.length === 0}
              onClick={() => step === prompts.length - 1 ? setShowResults(true) : setStep((current) => current + 1)}
              className="rounded-md bg-fd-primary px-4 py-2 text-sm font-medium text-fd-primary-foreground disabled:cursor-not-allowed disabled:opacity-40"
            >
              {step === prompts.length - 1 ? "Show my checklist" : "Next"}
            </button>
          </div>
        </>
      )}
    </section>
  );
}
