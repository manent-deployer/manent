import type { Config } from "tailwindcss";

// Colors are CSS variables (src/index.css), with a dark palette (default, like the
// documentation site) and a light one: components use only these names.
const token = (name: string) => `rgb(var(--${name}) / <alpha-value>)`;

export default {
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  darkMode: ["selector", '[data-theme="dark"]'],
  theme: {
    extend: {
      colors: {
        bg: token("bg"),
        surface: token("surface"),
        raised: token("raised"),
        border: token("border"),
        fg: token("fg"),
        muted: token("muted"),
        accent: token("accent"),
        "accent-fg": token("accent-fg"),
        violet: token("violet"),
        ok: token("ok"),
        warn: token("warn"),
        bad: token("bad"),
        info: token("info"),
      },
      fontFamily: {
        sans: ['"Inter Variable"', "system-ui", "-apple-system", '"Segoe UI"', "sans-serif"],
        display: ['"Space Grotesk"', '"Inter Variable"', "system-ui", "sans-serif"],
        mono: ['"JetBrains Mono Variable"', "ui-monospace", "SFMono-Regular", "Menlo", "monospace"],
      },
      borderRadius: { xl: "0.875rem" },
      boxShadow: { glow: "0 0 0 1px rgb(var(--accent) / 0.25), 0 8px 32px -12px rgb(var(--accent) / 0.35)" },
    },
  },
  plugins: [],
} satisfies Config;
