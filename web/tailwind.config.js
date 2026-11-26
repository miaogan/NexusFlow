/** @type {import('tailwindcss').Config} */
export default {
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  theme: {
    extend: {
      fontFamily: {
        sans: ["Inter", "PingFang SC", "Microsoft YaHei", "system-ui", "sans-serif"],
        mono: ["JetBrains Mono", "SFMono-Regular", "Menlo", "monospace"],
      },
      colors: {
        ink: {
          950: "#070b12",
          900: "#0b1220",
          800: "#111a2c",
          700: "#1a263c",
        },
        accent: {
          DEFAULT: "#38e8c6",
          dim: "#1f9e88",
        },
      },
    },
  },
  plugins: [],
};
