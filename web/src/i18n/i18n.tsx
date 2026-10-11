import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from "react";

import { en, type MessageKey } from "./en";
import { it } from "./it";

export type Lang = "en" | "it";
const dictionaries: Record<Lang, Record<MessageKey, string>> = { en, it };

export type T = (key: MessageKey, vars?: Record<string, string | number>) => string;

interface I18nValue {
  lang: Lang;
  locale: string;
  setLang: (l: Lang) => void;
  t: T;
}

const I18nContext = createContext<I18nValue | null>(null);

function initialLang(): Lang {
  try {
    const saved = window.localStorage.getItem("manent.lang");
    if (saved === "en" || saved === "it") return saved;
  } catch {
    // localStorage non disponibile: si usa la lingua del browser.
  }
  return navigator.language.toLowerCase().startsWith("it") ? "it" : "en";
}

/** format sostituisce {nome} con i valori indicati. */
export function format(text: string, vars?: Record<string, string | number>): string {
  if (!vars) return text;
  return text.replace(/\{(\w+)\}/g, (m, k: string) => (k in vars ? String(vars[k]) : m));
}

export function I18nProvider({ children, lang: forced }: { children: ReactNode; lang?: Lang }) {
  const [lang, setLangState] = useState<Lang>(() => forced ?? initialLang());
  useEffect(() => {
    document.documentElement.lang = lang;
  }, [lang]);
  const setLang = useCallback((l: Lang) => {
    setLangState(l);
    try {
      window.localStorage.setItem("manent.lang", l);
    } catch {
      // preferenza non salvata
    }
  }, []);
  const value = useMemo<I18nValue>(() => {
    const dict = dictionaries[lang];
    return {
      lang,
      locale: lang === "it" ? "it-IT" : "en-GB",
      setLang,
      t: (key, vars) => format(dict[key] ?? en[key] ?? key, vars),
    };
  }, [lang, setLang]);
  return <I18nContext.Provider value={value}>{children}</I18nContext.Provider>;
}

export function useI18n(): I18nValue {
  const ctx = useContext(I18nContext);
  if (!ctx) throw new Error("useI18n fuori da I18nProvider");
  return ctx;
}
