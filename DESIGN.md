---
name: ccc
description: One assistant, every subscription — a typical 2026 product page.
colors:
  fill: "#f5f5f4"
  on-fill: "#09090b"
  bg: "#09090b"
  bg-elev: "#111113"
  bg-pane: "#0c0c0e"
  ink: "#f5f5f4"
  ink-dim: "color-mix(in srgb, #f5f5f4 78%, #09090b)"
  ink-faint: "color-mix(in srgb, #f5f5f4 62%, #09090b)"
  line: "color-mix(in srgb, #f5f5f4 12%, #09090b)"
  tg-bg: "#0e1621"
  tg-head: "#17212b"
  tg-in: "#182533"
  tg-out: "#2b5278"
  tg-in-ink: "#dbe6f0"
  tg-out-ink: "#eef5fb"
  run: "#3dd68c"
  wait: "#e8b84a"
  idle: "color-mix(in srgb, #f5f5f4 38%, #09090b)"
  kbd-fill: "#25303d"
  kbd-ink: "#6cb7f0"
  avatar-fill: "#2a3f54"
  avatar-ink: "#d7e3ee"
typography:
  display:
    fontFamily: "Mona Sans, ui-sans-serif, system-ui, sans-serif"
    fontSize: "clamp(2.5rem, 6vw, 4.5rem)"
    fontWeight: 600
    lineHeight: 1.04
    letterSpacing: "-0.035em"
  headline:
    fontFamily: "Mona Sans, ui-sans-serif, system-ui, sans-serif"
    fontSize: "clamp(1.75rem, 3.2vw, 2.5rem)"
    fontWeight: 600
    lineHeight: 1.15
    letterSpacing: "-0.03em"
  title:
    fontFamily: "Mona Sans, ui-sans-serif, system-ui, sans-serif"
    fontSize: "1rem"
    fontWeight: 600
    lineHeight: 1.3
    letterSpacing: "-0.02em"
  body:
    fontFamily: "Mona Sans, ui-sans-serif, system-ui, sans-serif"
    fontSize: "1.0625rem"
    fontWeight: 400
    lineHeight: 1.55
    letterSpacing: "-0.011em"
  label:
    fontFamily: "Mona Sans, ui-sans-serif, system-ui, sans-serif"
    fontSize: "0.9375rem"
    fontWeight: 500
    lineHeight: 1
    letterSpacing: "-0.015em"
  mono:
    fontFamily: "Monaspace Neon, ui-monospace, monospace"
    fontSize: "0.84em"
    fontWeight: 450
    lineHeight: 1.7
rounded:
  xs: "4px"
  sm: "6px"
  md: "8px"
  lg: "12px"
  pill: "999px"
spacing:
  xs: "0.4rem"
  sm: "0.75rem"
  md: "1.5rem"
  lg: "2rem"
  xl: "3rem"
  section: "5.5rem"
  max: "72rem"
components:
  button-primary:
    backgroundColor: "{colors.fill}"
    textColor: "{colors.on-fill}"
    typography: "{typography.label}"
    rounded: "{rounded.md}"
    padding: "0 1rem"
    height: "2.5rem"
  button-primary-hover:
    backgroundColor: "{colors.fill}"
    textColor: "{colors.on-fill}"
    typography: "{typography.label}"
    rounded: "{rounded.md}"
    padding: "0 1rem"
    height: "2.5rem"
  button-primary-lg:
    backgroundColor: "{colors.fill}"
    textColor: "{colors.on-fill}"
    rounded: "{rounded.md}"
    padding: "0 1.25rem"
    height: "2.75rem"
  button-secondary:
    backgroundColor: "transparent"
    textColor: "{colors.ink}"
    typography: "{typography.label}"
    rounded: "{rounded.md}"
    padding: "0 1rem"
    height: "2.5rem"
  button-nav:
    backgroundColor: "{colors.fill}"
    textColor: "{colors.on-fill}"
    rounded: "{rounded.md}"
    padding: "0 0.75rem"
    height: "2rem"
  chip-status:
    backgroundColor: "transparent"
    textColor: "{colors.ink-dim}"
    rounded: "{rounded.pill}"
    padding: "0.12rem 0.4rem"
  window:
    backgroundColor: "{colors.bg-elev}"
    rounded: "{rounded.lg}"
  term:
    backgroundColor: "{colors.bg-elev}"
    textColor: "{colors.ink}"
    typography: "{typography.mono}"
    rounded: "{rounded.lg}"
  copy-btn:
    backgroundColor: "transparent"
    textColor: "{colors.ink}"
    rounded: "{rounded.sm}"
    padding: "0 0.7rem"
    height: "1.85rem"
  kbd-btn:
    backgroundColor: "{colors.kbd-fill}"
    textColor: "{colors.kbd-ink}"
    rounded: "{rounded.md}"
    padding: "0.4rem 0.6rem"
    height: "2.2rem"
---

# Design System: ccc

## Overview

**Creative North Star: "The Coding-Tool Product Page"**

ccc’s public face is a straight 2026 coding-tool landing: near-black ground, off-white ink, hairline rules, a white filled button with black type. It sits next to Cursor, Claude, and Codex in craft, not in costume. There is no governing metaphor — not radio, not ATC, not sewing — and no neo-terminal theatre. The product is shown the way those sites show an editor: a two-pane window, full content width, with the Telegram DM on the left and backend sessions on the right.

Personality is precise and quiet. Density is editorial, not dashboard: large type, long measure, generous section air, one signature object. Color on the page is almost only ink on ground; the Telegram blues and session greens live inside the product chrome as interface content. Motion is a single bubble rise and a running-state pulse, both off when the visitor asks for reduced motion.

**Key Characteristics:**
- Near-black page ground with off-white ink and 1px hairlines at ink-on-ground 12%
- Primary action is a filled off-white pill-rect with black type — no colored CTA
- Mona Sans for all UI; Monaspace Neon only for commands and inline code
- Product chrome is an OS-style window (traffic lights, titlebar, two panes)
- Telegram palette is nested interface, never page brand

## Colors

The page is a two-ink system (near-black / off-white). Hue appears only inside the product frame.

### Primary
- **Fill** (`fill`): The inverted action color. Filled buttons, skip-link chip, and the wordmark-on-dark pairing. Same value as page ink; used as a surface, not as text, when it is the CTA.

### Neutral
- **Ground** (`bg`): Page, sticky nav, and the field around the product window.
- **Pane** (`bg-pane`): Titlebar and the sessions column — one step up from ground.
- **Elevated** (`bg-elev`): Window and terminal bodies.
- **Ink** (`ink`): Headlines, wordmark, filled-button inverse, focus ring.
- **Ink dim** (`ink-dim`): Lede, body copy, nav links, footer.
- **Ink faint** (`ink-faint`): Captions, titlebar labels, engine slugs’ neighbors, session sublines.
- **Hairline** (`line`): 1px rules — nav bottom, pane split, engine row, footer, terminal border, status pill default.

### Interface (inside the product frame only)
- **Telegram ground / head / in / out** (`tg-bg`, `tg-head`, `tg-in`, `tg-out`): DM canvas, header, incoming bubble, outgoing bubble.
- **Bubble ink** (`tg-in-ink`, `tg-out-ink`): Message type on those bubbles.
- **Keyboard fill / ink** (`kbd-fill`, `kbd-ink`): Inline Telegram buttons (accounts, model picker).
- **Avatar fill / ink** (`avatar-fill`, `avatar-ink`): The `c` disc in the DM header.
- **Run / wait / idle** (`run`, `wait`, `idle`): Session dots and matching pill type. Not page accents.

### Named Rules
**The Frame Rule.** Telegram blues, keyboard cyan, avatar steel, and session status hues exist only inside product chrome. The page around the window stays near-black / off-white / hairline.

**The Fill Rule.** The only page-level accent is inverted fill: off-white surface, near-black type. Do not introduce a brand hue for buttons, links, or headlines.

## Typography

**Display Font:** Mona Sans (ui-sans-serif, system-ui, sans-serif)
**Body Font:** Mona Sans (same stack)
**Label/Mono Font:** Monaspace Neon (ui-monospace, monospace) — commands and code only

**Character:** GitHub’s product grotesk, slightly tight tracking, weight 600 for display. The mono face is a command prompt, not a second headline.

### Hierarchy
- **Display** (600, clamp 2.5–4.5rem, line-height 1.04, tracking −0.035em): Hero and closing headlines. Cap width around 14–16ch.
- **Headline** (600, clamp 1.75–2.5rem, line-height 1.15, tracking −0.03em): Section titles.
- **Title** (600, 1rem, tracking −0.02em): Quiet-grid headings and pane names.
- **Body** (400, 1.0625rem, line-height 1.55, tracking −0.011em): Running copy. Lede is the same face at 1.1875rem / 1.5, max-width 38–40rem, in ink-dim.
- **Label** (500, 0.9375rem, tracking −0.015em): Nav, buttons. Wordmark is 1.125rem / 600 / −0.04em, lowercase `ccc`.
- **Mono** (450, 0.84em relative to body; 0.9rem in the install block, line-height 1.7): Shell commands, engine slugs, `/session` and friends.

### Named Rules
**The Command Face Rule.** Monaspace Neon is for commands and code. It is not a headline, a label, or a “developer atmosphere” texture on body copy.

## Layout

A single centered column at `min(72rem, calc(100% - 2.5rem))`. Sticky nav is 3.5rem tall, 1.5rem side padding, full viewport width with a hairline floor. Hero opens with 4.5rem top padding (3rem on small screens); sections stack with 5.5rem top padding; the closing band is 6.5rem / 5rem and centered.

Two-column splits (copy | frame, quiet grid of three, engine row of four) share one collapse: at 860px every grid becomes one column and interior left-borders become top-borders. Nav text links hide; the filled GitHub control stays. Footer stacks. Product panes stack with the DM above sessions.

Rhythm is 0.6–0.75rem inside clusters, 1.5–2rem between related blocks, 3rem in the copy/frame split. No icon-card grid, no boxed feature tiles.

## Elevation & Depth

The page is flat: ground is one plane, structure is hairlines and one-step tonal shifts (`bg` → `bg-pane` → `bg-elev`). Depth is reserved for the product window.

### Shadow Vocabulary
- **Product window** (`box-shadow: 0 10px 28px -6px color-mix(in srgb, #000 62%, transparent)`): Soft, downward, only on the OS-style frame. Not on buttons, cards, or the terminal (the terminal uses a hairline instead).

### Named Rules
**The One Shadow Rule.** One ambient window shadow. No glow, no grid overlay, no stacked drop shadows on controls.

## Shapes

Controls are 8px rectangles (buttons, skip link, keyboard buttons). Frames are 12px (product window, terminal, chat bubbles) with overflow clipped. Bubbles keep a 4px inner-corner on the stem side. Composer and status pills are fully round (`999px`). Traffic lights and the DM avatar are circles. Hairlines are 1px, never 2px, never dashed.

Favicon is an 8px-rounded square of ground with the wordmark in ink — the same two-ink, 8px control language at 32px.

## Components

Restrained product chrome: filled or hairline, no color fills on the page, no lift on hover.

### Buttons
- **Shape:** 8px corners; inline-flex; height 2.5rem; padding 0 1rem; weight 500.
- **Primary:** Fill on near-black type. Hover darkens with `filter: brightness(0.94)` — no translate, no shadow.
- **Large:** Height 2.75rem, padding 0 1.25rem, 1rem type — closing CTA only in the current build.
- **Secondary:** Transparent, ink type, hairline border. Hover is a 6% ink wash on ground.
- **Nav:** Same fill as primary, shorter (2rem / 0.8125rem type).
- **Focus:** 2px ink ring, 3px offset, on every control.

### Chips
- **Style:** Lowercase status pills — hairline, 0.6875rem, 500, tracking 0.02em, fully round.
- **State:** Running uses `run` type and a 35% run border; waiting uses `wait` the same way; idle stays dim ink on the default hairline.

### Cards / Containers
- **Product window:** 12px, elevated fill, one ambient shadow, traffic-light titlebar on pane ground, two panes (1.2fr | 0.8fr) split by a hairline.
- **Terminal:** 12px, elevated fill, hairline border, no shadow; bar 2.65rem with a small copy control (6px, 1.85rem).
- **Engine row:** Not cards — a hairline-top-and-bottom strip of four cells.

### Inputs / Fields
- **Composer:** Fully round bar in Telegram head color, placeholder at 72% white — display only in the current build.
- **Copy control:** Hairline, 6px corners; hover is a 6% ink wash.

### Navigation
- Sticky, ground-filled, hairline floor. Wordmark left; dim 0.9375rem links that go full ink on hover; filled GitHub right. Below 860px, only the wordmark and GitHub remain.

### Product frame (signature)
OS window around a Telegram DM and a session list. Titlebar: three 0.65rem lights (#ff5f57, #febc2e, #28c840) and a faint 0.75rem name. DM uses the Telegram tokens; outgoing bubbles sit right with a 4px bottom-right corner; incoming sit left. Sessions live on pane ground with 0.5rem status dots (running pulses 2.2s). `ask_owner` is the same window with listed options in the incoming bubble, not a keyboard.

Bubbles enter with `rise` (0.55s, cubic-bezier(0.16, 1, 0.3, 1), 8px up). `prefers-reduced-motion: reduce` kills bubble and pulse animation.

## Do's and Don'ts

### Do:
- **Do** keep page chrome on ground / ink / hairline / fill, and put Telegram and status color only inside the product frame (The Frame Rule).
- **Do** set primary actions in fill with on-fill type at 8px radius and 2.5rem height.
- **Do** set Mona Sans for UI and Monaspace Neon for commands only (The Command Face Rule).
- **Do** show the product as a two-pane window with one ambient shadow (The One Shadow Rule).
- **Do** collapse multi-column layouts to one column at 860px, turning left hairlines into top hairlines.

### Don't:
- **Don't** use Telegram blue, keyboard cyan, or session green as page-level brand, link, or button color.
- **Don't** add glow, grid overlay, gradient text, or an icon-card feature grid.
- **Don't** dress the page as a terminal (scanlines, phosphor, prompt-as-headline) or as a radio / ATC / sewing world.
- **Don't** put a second drop shadow on buttons or the install terminal — the window already carries the only shadow.
- **Don't** uppercase labels or buttons; status pills stay lowercase.
