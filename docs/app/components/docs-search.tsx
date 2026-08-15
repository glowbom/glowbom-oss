import { buttonVariants } from "fumadocs-ui/components/ui/button";
import { Search, X } from "lucide-react";
import {
  createContext,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type KeyboardEvent as ReactKeyboardEvent,
  type ReactNode,
} from "react";
import { useNavigate } from "react-router";
import type { SearchRecord } from "../lib/docs";

type DocsSearchContextValue = {
  modalOpen: boolean;
  modifierKey: string;
  records: SearchRecord[];
  selectResult: (result: SearchRecord) => void;
  setModalOpen: (value: boolean) => void;
};

const DocsSearchContext = createContext<DocsSearchContextValue | null>(null);

function useDocsSearchContext() {
  const value = useContext(DocsSearchContext);

  if (!value) {
    throw new Error("DocsSearch components must be used inside DocsSearchProvider.");
  }

  return value;
}

function normalize(value: string) {
  return value.toLowerCase().replace(/\s+/g, " ").trim();
}

function scoreRecord(record: SearchRecord, query: string) {
  if (query.length === 0) {
    return record.type === "page" ? 100 : 0;
  }

  const haystack = normalize(record.text);
  const content = normalize(record.content);

  if (!haystack.includes(query)) {
    return 0;
  }

  let score = 0;

  if (content === query) score += 120;
  else if (content.startsWith(query)) score += 90;
  else if (content.includes(query)) score += 70;
  else score += 40;

  if (record.type === "page") score += 30;
  if (record.type === "heading") score += 15;
  if (record.type === "text") score += 5;

  return score;
}

function useSearchResults(records: SearchRecord[], search: string) {
  return useMemo(() => {
    const query = normalize(search);

    return records
      .map((record) => ({
        record,
        score: scoreRecord(record, query),
      }))
      .filter((item) => item.score > 0)
      .sort((a, b) => b.score - a.score)
      .slice(0, query.length === 0 ? 8 : 12)
      .map((item) => item.record);
  }, [records, search]);
}

function getNextIndex(currentIndex: number, delta: number, total: number) {
  if (total === 0) {
    return 0;
  }

  return (currentIndex + delta + total) % total;
}

function SearchResultItem({
  active,
  onClick,
  onMouseEnter,
  result,
}: {
  active: boolean;
  onClick: () => void;
  onMouseEnter: () => void;
  result: SearchRecord;
}) {
  return (
    <button
      type="button"
      aria-selected={active}
      className={`relative flex w-full shrink-0 flex-col gap-1 overflow-hidden rounded-lg px-3 py-2 text-left text-sm transition-colors ${
        active ? "bg-fd-accent text-fd-accent-foreground" : "hover:bg-fd-accent/60"
      }`}
      onClick={onClick}
      onMouseEnter={onMouseEnter}
    >
      {result.breadcrumbs.length > 0 ? (
        <div className="truncate text-xs text-fd-muted-foreground">{result.breadcrumbs.join(" / ")}</div>
      ) : null}
      <div className="truncate font-medium">
        {result.type === "heading" ? "# " : ""}
        {result.content}
      </div>
    </button>
  );
}

function SearchResultsList({
  activeIndex,
  emptyLabel,
  onSelect,
  results,
  setActiveIndex,
}: {
  activeIndex: number;
  emptyLabel: string;
  onSelect: (result: SearchRecord) => void;
  results: SearchRecord[];
  setActiveIndex: (value: number) => void;
}) {
  if (results.length === 0) {
    return <div className="px-4 py-10 text-center text-sm text-fd-muted-foreground">{emptyLabel}</div>;
  }

  return (
    <div className="flex max-h-[420px] flex-col overflow-y-auto p-2">
      {results.map((result, index) => (
        <SearchResultItem
          key={result.id}
          active={index === activeIndex}
          onClick={() => onSelect(result)}
          onMouseEnter={() => setActiveIndex(index)}
          result={result}
        />
      ))}
    </div>
  );
}

function useGlobalShortcut(setModalOpen: (value: boolean) => void) {
  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      const target = event.target;
      const isEditable =
        target instanceof HTMLElement &&
        (target.isContentEditable ||
          target.tagName === "INPUT" ||
          target.tagName === "TEXTAREA" ||
          target.tagName === "SELECT");

      if (!isEditable && event.key === "/") {
        event.preventDefault();
        setModalOpen(true);
        return;
      }

      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k") {
        event.preventDefault();
        setModalOpen(true);
      }
    };

    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [setModalOpen]);
}

export function DocsSearchProvider({
  children,
  records,
}: {
  children: ReactNode;
  records: SearchRecord[];
}) {
  const navigate = useNavigate();
  const [modalOpen, setModalOpen] = useState(false);
  const [modifierKey, setModifierKey] = useState("Ctrl");

  useEffect(() => {
    if (window.navigator.userAgent.includes("Mac")) {
      setModifierKey("⌘");
    }
  }, []);

  useGlobalShortcut(setModalOpen);

  const value = useMemo<DocsSearchContextValue>(
    () => ({
      modalOpen,
      modifierKey,
      records,
      selectResult: (result) => {
        setModalOpen(false);
        navigate(result.url);
      },
      setModalOpen,
    }),
    [modalOpen, modifierKey, navigate, records],
  );

  return <DocsSearchContext.Provider value={value}>{children}</DocsSearchContext.Provider>;
}

export function DocsSearchLargeTrigger() {
  const { modifierKey, records, selectResult } = useDocsSearchContext();
  const [open, setOpen] = useState(false);
  const [search, setSearch] = useState("");
  const [activeIndex, setActiveIndex] = useState(0);
  const containerRef = useRef<HTMLDivElement | null>(null);
  const results = useSearchResults(records, search);

  useEffect(() => {
    setActiveIndex(0);
  }, [search, results.length]);

  useEffect(() => {
    const onPointerDown = (event: PointerEvent) => {
      if (!containerRef.current?.contains(event.target as Node)) {
        setOpen(false);
      }
    };

    document.addEventListener("pointerdown", onPointerDown);
    return () => document.removeEventListener("pointerdown", onPointerDown);
  }, []);

  const onKeyDown = (event: ReactKeyboardEvent<HTMLInputElement>) => {
    if (event.key === "ArrowDown") {
      event.preventDefault();
      setOpen(true);
      setActiveIndex((current) => getNextIndex(current, 1, results.length));
      return;
    }

    if (event.key === "ArrowUp") {
      event.preventDefault();
      setOpen(true);
      setActiveIndex((current) => getNextIndex(current, -1, results.length));
      return;
    }

    if (event.key === "Escape") {
      setOpen(false);
      return;
    }

    if (event.key === "Enter") {
      const selected = results[activeIndex] ?? results[0];

      if (!selected || search.trim().length === 0) {
        return;
      }

      event.preventDefault();
      setOpen(false);
      selectResult(selected);
    }
  };

  return (
    <div className="relative" ref={containerRef}>
      <div className="inline-flex w-full items-center gap-2 rounded-lg border bg-fd-secondary/50 p-1.5 ps-2 text-sm text-fd-muted-foreground transition-colors hover:bg-fd-accent hover:text-fd-accent-foreground">
        <Search className="size-4 shrink-0" />
        <input
          className="min-w-0 flex-1 bg-transparent text-fd-foreground placeholder:text-fd-muted-foreground focus:outline-none"
          onChange={(event) => {
            setSearch(event.target.value);
            setOpen(true);
          }}
          onFocus={() => setOpen(true)}
          onKeyDown={onKeyDown}
          placeholder="Search docs"
          value={search}
        />
        <div className="ms-auto inline-flex gap-0.5">
          <kbd className="rounded-md border bg-fd-background px-1.5">{modifierKey}</kbd>
          <kbd className="rounded-md border bg-fd-background px-1.5">K</kbd>
        </div>
      </div>
      {open ? (
        <div className="absolute inset-x-0 top-[calc(100%+0.5rem)] z-40 overflow-hidden rounded-xl border bg-fd-popover text-fd-popover-foreground shadow-xl">
          <SearchResultsList
            activeIndex={activeIndex}
            emptyLabel={search.trim().length === 0 ? "Start typing to search the docs." : "No matching docs found."}
            onSelect={(result) => {
              setOpen(false);
              selectResult(result);
            }}
            results={results}
            setActiveIndex={setActiveIndex}
          />
        </div>
      ) : null}
    </div>
  );
}

export function DocsSearchSmallTrigger() {
  const { setModalOpen } = useDocsSearchContext();

  return (
    <button
      type="button"
      aria-label="Open Search"
      className={buttonVariants({ color: "ghost", size: "icon-sm", className: "p-2" })}
      onClick={() => setModalOpen(true)}
    >
      <Search className="size-4" />
    </button>
  );
}

export function DocsSearchDialog() {
  const { modalOpen, records, selectResult, setModalOpen } = useDocsSearchContext();
  const [search, setSearch] = useState("");
  const [activeIndex, setActiveIndex] = useState(0);
  const inputRef = useRef<HTMLInputElement | null>(null);
  const results = useSearchResults(records, search);

  useEffect(() => {
    setActiveIndex(0);
  }, [search, results.length]);

  useEffect(() => {
    if (modalOpen) {
      window.setTimeout(() => inputRef.current?.focus(), 0);
    } else {
      setSearch("");
    }
  }, [modalOpen]);

  if (!modalOpen) {
    return null;
  }

  const onKeyDown = (event: ReactKeyboardEvent<HTMLInputElement>) => {
    if (event.key === "ArrowDown") {
      event.preventDefault();
      setActiveIndex((current) => getNextIndex(current, 1, results.length));
      return;
    }

    if (event.key === "ArrowUp") {
      event.preventDefault();
      setActiveIndex((current) => getNextIndex(current, -1, results.length));
      return;
    }

    if (event.key === "Escape") {
      setModalOpen(false);
      return;
    }

    if (event.key === "Enter") {
      const selected = results[activeIndex] ?? results[0];

      if (!selected || search.trim().length === 0) {
        return;
      }

      event.preventDefault();
      selectResult(selected);
    }
  };

  return (
    <>
      <button
        aria-label="Close Search"
        className="fixed inset-0 z-40 bg-fd-background/70 backdrop-blur-sm"
        onClick={() => setModalOpen(false)}
        type="button"
      />
      <div className="fixed left-1/2 top-4 z-50 w-[calc(100%-1rem)] max-w-screen-sm -translate-x-1/2 overflow-hidden rounded-xl border bg-fd-popover text-fd-popover-foreground shadow-2xl md:top-[calc(50%-250px)]">
        <div className="flex items-center gap-2 border-b p-3">
          <Search className="size-5 text-fd-muted-foreground" />
          <input
            ref={inputRef}
            className="w-0 flex-1 bg-transparent text-lg placeholder:text-fd-muted-foreground focus:outline-none"
            onChange={(event) => setSearch(event.target.value)}
            onKeyDown={onKeyDown}
            placeholder="Search docs"
            value={search}
          />
          <button
            type="button"
            className={buttonVariants({
              color: "outline",
              size: "sm",
              className: "font-mono text-fd-muted-foreground",
            })}
            onClick={() => setModalOpen(false)}
          >
            <X className="size-4" />
          </button>
        </div>
        <SearchResultsList
          activeIndex={activeIndex}
          emptyLabel={search.trim().length === 0 ? "Start typing to search the docs." : "No matching docs found."}
          onSelect={selectResult}
          results={results}
          setActiveIndex={setActiveIndex}
        />
        <div className="border-t bg-fd-secondary/50 p-3 text-xs text-fd-muted-foreground">
          Search across Glowbom docs, including Glowbom OSS.
        </div>
      </div>
    </>
  );
}
