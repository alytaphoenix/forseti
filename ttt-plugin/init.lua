-- Forseti ttt plugin (Phase 2a + 2c)
-- Ties ttt into herdr + pi:
--   Forseti: Jump      (palette cmd) — pi wrote {path,line,end_line} to jump.json
--   Forseti: Review    (palette cmd) — pi wrote review.json (turn edit list)
--   forseti.ask        (ctrl+k a)    — fixed quick-ask with context
--   sidebar "Forseti"  — status + an input row: "ask pi about this …" with a
--                        real question; post-submit herdr agent focus ring.
--   context.json       — written on cursor/change/file events; the pi side
--                        reads it to become IDE-aware in every prompt.
-- Design: docs/design.md §2 · verified APIs: docs/spikes.md · plan: docs/implementation-plan.md
local ttt = require("ttt")
local json = require("ttt.json")
local fs = require("ttt.fs")
local sys = require("ttt.system")
local editor = require("ttt.editor")

local JUMP_FILE = ttt.plugin_dir() .. "/jump.json"
local CONTEXT_FILE = ttt.plugin_dir() .. "/context.json"
local REVIEW_FILE = ttt.plugin_dir() .. "/review.json"
local POLL_MS = 3000

local agents = {}          -- { {name, status, workspace_id}, ... }
local last_error = ""
local auto_focus = true    -- after ask, focus the pi pane to watch the answer
local last_ctx_write = 0

local function log_err(msg) ttt.log("error", "forseti: " .. msg) end

-- --- herdr plumbing --------------------------------------------------------

local function herdr_cmd(args)
  local ok, r = pcall(sys.exec, "herdr", args)
  if not ok or not r or not r.stdout or r.stdout == "" then return nil end
  return r.stdout
end

local function parse_agents(raw)
  local pi = {}
  -- kind-first (observed 0.9.3 key order); [^{}]- stays inside one flat object
  for kind, status, name, ws in raw:gmatch(
        '"agent":"([%w%-_]+)","agent_status":"([%w_]+)"[^{}]-"name":"([^"]+)"[^{}]-"workspace_id":"([^"]+)"') do
    if kind == "pi" then pi[#pi + 1] = { name = name, status = status, workspace_id = ws } end
  end
  if #pi == 0 then -- fallback: reversed order (future-proof)
    for name, status, ws in raw:gmatch(
          '"name":"([^"]+)"[^{}]-"agent_status":"([%w_]+)"[^{}]-"workspace_id":"([^"]+)"') do
      pi[#pi + 1] = { name = name, status = status or "unknown", workspace_id = ws }
    end
  end
  return pi
end

local function pi_agents()
  local out = herdr_cmd({ "agent", "list" })
  if not out then return {} end
  return parse_agents(out)
end

local function resolve_agent()
  agents = pi_agents()
  local mine = (pcall(sys.env, "HERDR_WORKSPACE_ID") and sys.env("HERDR_WORKSPACE_ID")) or ""
  if #agents == 0 then return nil, "no live pi agent — run forseti:open (herdr plugin) first" end
  local hits = {}
  for _, a in ipairs(agents) do
    if mine ~= "" and a.workspace_id == mine then hits[#hits + 1] = a end
  end
  if #hits == 1 then return hits[1] end
  if #hits > 1 then return nil, "ambiguous: " .. #hits .. " pi agents in this workspace" end
  if #agents == 1 then return agents[1] end
  return nil, #agents .. " pi agents live and none is in this workspace — ambiguous target"
end

-- --- editor context (what is the user looking at right now) ---------------

local function write_context()
  local ok_sel, path = pcall(editor.file_path)
  if not ok_sel or not path or path == "" then return end
  local now_secs = os.time()
  if now_secs - last_ctx_write < 1 then return end -- throttle storms
  last_ctx_write = now_secs
  local data = { path = path, ts = now_secs }
  local pos_ok, cur = pcall(editor.cursor)
  if pos_ok and cur then data.line, data.col = cur.line, cur.col end
  local sel_ok, sel = pcall(editor.selection)
  if sel_ok and sel and sel.active then
    data.sel_start, data.sel_end = sel.start_line, sel.end_line
    local text_ok, stext = pcall(editor.selection_text)
    if text_ok and stext then data.selection = stext end
  end
  local ok, encoded = pcall(json.encode, data)
  if ok then fs.write(CONTEXT_FILE, encoded) end
end

-- --- ask -------------------------------------------------------------------

local function clip(s, n)
  if #s <= n then return s end
  return s:sub(1, n) .. "\n… (clipped)"
end

local function editor_snippet()
  local path = (pcall(editor.file_path) and editor.file_path()) or "<unsaved buffer>"
  local cur = (pcall(editor.cursor) and editor.cursor()) or { line = 1, col = 1 }
  local sel = pcall(editor.selection) and editor.selection()
  local snippet, loc
  if sel and sel.active then
    snippet = editor.selection_text()
    loc = string.format("lines %d–%d", sel.start_line, sel.end_line)
  else
    snippet = editor.current_line()
    loc = "line " .. cur.line
  end
  return path, loc, clip(snippet or "", 3000)
end

local function submit_prompt(a, prompt, label)
  local out = herdr_cmd({ "agent", "prompt", a.name, prompt })
  if not out then
    last_error = "herdr agent prompt failed"
    log_err(last_error)
    return false
  end
  last_error = ""
  ttt.set_status_item("left", "ask", "asked pi (" .. a.name .. "): " .. label)
  ttt.set_timeout(2500, function() ttt.remove_status_item("ask") end)
  if auto_focus then
    pcall(sys.exec, "herdr", { "agent", "focus", a.name })
  end
  return true
end

local function quick_ask()
  local ok, a_or_err = pcall(resolve_agent)
  if not ok or not a_or_err then last_error = tostring(a_or_err); log_err(last_error); return end
  local path, loc, snippet = editor_snippet()
  local prompt = string.format(
    'Look at %s (%s):\n"""\n%s\n"""\nExplain what this does and point out any problems, concisely. Do not edit anything yet.',
    path, loc, snippet)
  submit_prompt(a_or_err, prompt, "quick ask")
end

local function ask_with_question(question)
  local ok, a_or_err = pcall(resolve_agent)
  if not ok or not a_or_err then last_error = tostring(a_or_err); log_err(last_error); return end
  local path, loc, snippet = editor_snippet()
  local prompt = string.format(
    '[From ttt: %s (%s)]\n"""\n%s\n"""\n\n%s\n(Do not edit anything yet.)',
    path, loc, snippet, clip(question, 2000))
  submit_prompt(a_or_err, prompt, question)
end

-- --- Forseti: Jump / Review ------------------------------------------------

local function jump()
  local content = fs.read(JUMP_FILE)
  if not content then log_err("no jump request file at " .. JUMP_FILE) return end
  local ok, data = pcall(json.decode, content)
  if not ok or type(data) ~= "table" or not data.path then
    log_err("jump.json unreadable or missing 'path'"); return
  end
  local line = tonumber(data.line)
  if line and line > 0 then ttt.open_file(data.path, line)
  else ttt.open_file(data.path) end
  if line and tonumber(data.end_line) and data.end_line > line then
    editor.set_selection(line, 1, tonumber(data.end_line), 1)
  end
  last_error = ""
end

local function review()
  local content = fs.read(REVIEW_FILE)
  if not content or content == "" then log_err("no review summary"); return end
  local ok, data = pcall(json.decode, content)
  if not ok or type(data) ~= "table" or type(data.files) ~= "table" or #data.files == 0 then
    log_err("review.json has no files"); return
  end
  if ttt.open_tab then
    ttt.open_tab({
      title = "Forseti: Review",
      render = function(panel)
        panel:label("changes made by pi this turn:")
        panel:label("")
        for _, f in ipairs(data.files) do
          panel:label(string.format("%s :%d", f.path or "?", tonumber(f.line) or 1))
        end
        panel:label("")
        panel:label("(first change opened; use Forseti: Jump per file)")
      end,
    })
  end
  local first = data.files[1]
  ttt.open_file(first.path, tonumber(first.line) or 1)
end

-- --- Forseti: Review (turn summary from the pi side) ----------------------
local function review()
  local content = fs.read(REVIEW_FILE)
  if not content or content == "" then log_err("no review summary at " .. REVIEW_FILE) return end
  local ok, data = pcall(json.decode, content)
  if not ok or type(data) ~= "table" or type(data.files) ~= "table" or #data.files == 0 then
    log_err("review.json empty or malformed"); return
  end
  if ttt.open_tab then
    ttt.open_tab({
      title = "Forseti: Review",
      render = function(panel)
        panel:label("changes made by pi this turn:")
        panel:label("")
        for _, f in ipairs(data.files) do
          panel:label("- " .. (f.path or "?") .. " :" .. (tonumber(f.line) or 1))
        end
        panel:label("")
        panel:label("Forseti: Jump nudges you back to the first change")
      end,
    })
  end
  local first = data.files[1]
  ttt.open_file(first.path, tonumber(first.line) or 1)
end

-- --- Phase 4-4: vault commands (evergreen secondbrain) ---------------------

local VAULT = nil
local fs_api_ready = true
do
  local vf = ttt.plugin_dir() .. "/vault.json"
  local ok_r, content, rerr = pcall(fs.read, vf)
  if not ok_r or not content then
    fs_api_ready = false
    ttt.log("warn", ("forseti: fs not usable yet (err=%s) — vault features disabled; retry after reload")
      :format(tostring(ok_r and rerr or ok_r and "unknown" or "call failed")))
  else
    local ok, data = pcall(json.decode, content)
    if ok and type(data) == "table" and data.vault then VAULT = data.vault
    else ttt.log("warn", "forseti: vault.json decode failed: " .. tostring(content)) end
  end
end

local function today()
  return os.date("%Y-%m-%d")
end

local function vault_ready()
  if not VAULT or not fs_api_ready then
    last_error = "vault not ready yet (ttt fs API loads after WirePlugin) — retry in a second, or pick it again from the palette"
    return false
  end
  if not fs.exists(VAULT .. "/notes") then
    last_error = "vault not accessible from this workspace (needs scripts/vault-init.sh, or open via forseti.open)"
    log_err(last_error)
    return false
  end
  return true
end

local function daily_note()
  if not vault_ready() then return end
  local file = VAULT .. "/daily/" .. today() .. ".md"
  if not fs.exists(file) then
    local tpl = fs.read(VAULT .. "/_templates/daily.md")
    if not tpl then log_err("missing daily template"); return end
    fs.write(file, (tpl:gsub("__TODAY__", today())))
  end
  ttt.log("info", "forseti: daily → " .. file)
  ttt.open_file(file, 1)
  ttt.set_status_item("left", "ask", "daily note open")
  ttt.set_timeout(2000, function() ttt.remove_status_item("ask") end)
end

local function slugify(t)
  local s = t:lower():gsub("[^%w%-]+", "-"):gsub("^-+", ""):gsub("-+$", "")
  return s
end

-- resolve a [[wikilink]] slug (or substring of one) to a vault file
local function resolve_link(slug)
  local candidates = { VAULT .. "/notes/" .. slug .. ".md", VAULT .. "/daily/" .. slug .. ".md", VAULT .. "/" .. slug .. ".md" }
  for _, c in ipairs(candidates) do
    if fs.exists(c) then return c end
  end
  return nil
end

local function wikilink_jump()
  local cur = editor.cursor()
  local line = editor.get_line(cur.line) or ""
  local col = cur.col
  -- find the [[…]] whose span covers the cursor (byte scan; 1-based cols)
  local best_s, best_text
  for s, text in line:gmatch("%[%[(.-)%]%]") do
    local e = line:find(text, s, true)
    -- find() re-scans; recompute span properly below
    local bs, be, inner = line:find("%[%[(.-)%]%]")
    -- FALLBACK below; a precise span pass is done second
    if not best_text then best_text = inner end -- first link this line (approximate)
    best_s = bs
  end
  -- precise pass: longest prefix match
  local span_s, span_e, inner = line:find("%[%[(.-)%]%]")
  local function span_at(i)
    local from = 1
    while true do
      local s, e = line:find("%[%[(.-)%]%]", from)
      if not s then return nil end
      if i >= s and i <= e + 0 then return s, e, line:match("^(.-)%]%]", s) end
      from = s + 1
    end
  end
  local s, e, link = span_at(col)
  if not link then
    for l2 in line:gmatch("%[%[(.-)%]%]") do link = l2 break end
  end
  if not link then log_err("cursor is not on a [[wikilink]]") return end
  local file = resolve_link(link)
  if not file then log_err("[[" .. link .. "]] not found in vault (notes/? daily/? root)") return end
  ttt.open_file(file, 1)
end

local function backlinks()
  local path = editor.file_path()
  if not path or not vault_ready() then return end
  local stem = path:match("([^/]+)%.md$") or path
  local stem_title = stem:gsub("^%-", ""):gsub("%-", " ")
  ttt.log("info", ("forseti: backlinks scan for %s (stem=%s)"):format(path, stem))
  local hits = {}
  for _, dir in ipairs({ VAULT .. "/notes", VAULT .. "/daily" }) do
    for _, entry in ipairs(fs.list(dir) or {}) do
      if not entry.is_dir and entry.name:match("%.md$") then
        local f = dir .. "/" .. entry.name
        if f ~= path then
          local content = fs.read(f)
          if content and (content:find("%[%[" .. stem .. "%]%]", 1, true) or content:find(stem, 1, true)) then
            hits[#hits + 1] = f
          end
        end
      end
    end
  end
  if ttt.open_tab then
    ttt.open_tab({
      title = "Forseti: Backlinks",
      render = function(panel)
        if #hits == 0 then panel:label("no backlinks found for " .. stem) return end
        panel:label("notes linking here (" .. stem .. "):")
        panel:label("")
        for _, h in ipairs(hits) do panel:label("- " .. h) end
      end,
    })
  end
  if hits[1] then ttt.open_file(hits[1], 1) end
end

local function open_obsidian()
  if not vault_ready() then log_err("vault not present — nothing to open") return end
  local vname = VAULT:match("([^/]+)$")
  local rel = ""
  local p = editor.file_path()
  if p and p:sub(1, #VAULT + 1) == VAULT .. "/" then
    rel = p:sub(#VAULT + 2)
    rel = rel:gsub("%%", "%%25"):gsub("%s", "%%20")
  end
  local uri = "obsidian://open?vault=" .. vname .. (rel ~= "" and (rel:find("^daily/") or rel:find("^notes/")) and "&file=" .. rel or "")
  -- macOS: `open <uri>`; linux fallback binary pointed at by $FORSETI_OPEN
  local opener = sys.env("FORSETI_OPEN") ~= "" and sys.env("FORSETI_OPEN") or "open"
  pcall(sys.exec, opener, { uri })
  ttt.set_status_item("left", "ask", "opened in obsidian: " .. (rel ~= "" and rel or vname))
  ttt.set_timeout(2500, function() ttt.remove_status_item("ask") end)
end

-- --- sidebar ---------------------------------------------------------------

local glyph = { idle = "●", working = "◐", blocked = "!", done = "✓", unknown = "?" }

local function render(panel)
  if last_error ~= "" then
    panel:label("! " .. last_error)
    panel:label("")
  end
  panel:input({
    placeholder = "ask pi about this… (enter)",
    prefix = "",
    on_submit = function(text)
      if text ~= "" then ask_with_question(text) end
    end,
  })
  panel:label("")
  if #agents == 0 then
    panel:label("no pi agents live")
    panel:label("run: herdr plugin action")
    panel:label("     invoke forseti.open")
    return
  end
  for _, a in ipairs(agents) do
    local g = glyph[a.status] or "?"
    local txt = g .. " " .. a.name .. "  " .. a.status
    if a.status == "blocked" then txt = txt .. "  (approval UI)" end
    panel:label(txt)
  end
  panel:label("")
  panel:label("ctrl+k a  quick ask")
  panel:label("type above: custom ask")
end


ttt.set_interval(POLL_MS, function()
  local prev = agents
  agents = pi_agents()
  for _, a in ipairs(agents) do
    local was
    for _, p in ipairs(prev) do if p.name == a.name then was = p end end
    if was and was.status == "working" and a.status ~= "working" then
      if a.status == "blocked" then
        ttt.set_status_item("right", "agent-" .. a.name, "! " .. a.name .. " needs you")
      else
        ttt.set_status_item("right", "agent-" .. a.name, a.name .. " " .. (glyph[a.status] or ""))
      end
    end
  end
end)

-- IDE-awareness: keep context.json fresh for the pi side (Phase 2c-3)
local events_ok, events = pcall(require, "ttt.events")
if events_ok then
  events.on("cursor.change", function(_path) write_context() end)
  events.on("file.open", function(_path) write_context() end)
  events.on("file.save", function(_path) write_context() end)
else
  log_err("ttt.events not available; IDE context disabled")
end

-- fs_ready: lazy retry of the filesystem API (U2 startup-order quirk —
-- LoadAll wires plugins before the FilesystemAPI exists; a later tick finds
-- it wired). Re-reads the vault state file once fs comes up.
local function fs_ready()
  if fs_api_ready then return true end
  local ok_r, content = pcall(fs.read, ttt.plugin_dir() .. "/vault.json")
  if ok_r and type(content) == "string" then
    fs_api_ready = true
    local ok, data = pcall(json.decode, content)
    if ok and type(data) == "table" and data.vault then VAULT = data.vault end
    ttt.log("info", "forseti: fs API came up on retry — vault + git features enabled")
    return true
  end
  return false
end

-- --- crew status bridge (Phase 6, 6B-5) -------------------------------------
-- The crew runner writes <repo>/.forseti/crew-status.json on every event and
-- removes it at teardown. Poll it here and surface a status badge:
--   "crew demo 1/3 ●" while running, "crew demo 2/3 !blocked" when a node
-- needs attention, badge cleared when the file is gone. Notifications are
-- disabled on this setup, so this badge IS the alert path (S14/AGENTS.md).
-- repo.json is read lazily inside the first tick: the fs API is not wired at
-- plugin-load time (same startup-order quirk the vault state file hits).
local REPO_FILE = ttt.plugin_dir() .. "/repo.json"
local CREW_REPO = nil

local function crew_render(st)
  local glyph = "●"
  if (st.blocked or 0) > 0 then glyph = "!"
  elseif (st.failed or 0) > 0 then glyph = "✗" end
  return string.format("crew %s %d/%d %s", st.crew or "?", st.done or 0, st.total or 0, glyph)
end

ttt.set_interval(2000, function()
  if CREW_REPO == nil then
    local ok_r, content = pcall(fs.read, REPO_FILE)
    if ok_r and type(content) == "string" then
      local ok, data = pcall(json.decode, content)
      if ok and type(data) == "table" and data.repo then
        CREW_REPO = data.repo
        ttt.log("info", "forseti: crew status bridge on (" .. CREW_REPO .. ")")
      else
        CREW_REPO = false -- present but unusable; stop retrying
      end
    end
    if CREW_REPO == nil then return end -- fs not wired yet; retry next tick
  end
  if CREW_REPO == false then return end
  local ok_r, content = pcall(fs.read, CREW_REPO .. "/.forseti/crew-status.json")
  if ok_r and type(content) == "string" then
    local ok, st = pcall(json.decode, content)
    if ok and type(st) == "table" then
      ttt.set_status_item("right", "crew", crew_render(st))
      return
    end
  end
  ttt.remove_status_item("crew") -- no live run (file removed at teardown)
end)


-- --- Phase 7: git surface (lazygit pane + ask-about-diff) --------------------

-- P7-2: open/focus the lazygit pane via the herdr plugin action (git.sh does
-- the focus-or-create; ttt only re-dispatches — no TUI nesting here).
local function open_git()
  local out = herdr_cmd({ "plugin", "action", "invoke", "forseti.git" })
  if not out then
    ttt.set_status_item("left", "ask", "forseti: git action failed (herdr up?)")
    ttt.set_timeout(3000, function() ttt.remove_status_item("ask") end)
    return
  end
  local state = out:match('"forseti":"focused"') and "focused" or "opened"
  ttt.set_status_item("left", "ask", "forseti: lazygit " .. state)
  ttt.set_timeout(2500, function() ttt.remove_status_item("ask") end)
end

-- P7-5: ask pi about the working tree's uncommitted changes.
-- git runs via the manifest allowlist ("git"); the diff excerpt caps at ~2k
-- chars so the prompt stays small. Repo root resolves from the open file.
local function ask_diff()
  if not fs_ready() then
    ttt.set_status_item("left", "ask", "forseti: fs not ready — retry")
    ttt.set_timeout(3000, function() ttt.remove_status_item("ask") end)
    return
  end
  local ok_p, cur = pcall(editor.file_path)
  if not ok_p or not cur or not cur:find("/repos/") then
    log_err(("ask_diff: no repo file open (ok_p=%s path=%s)"):format(tostring(ok_p), tostring(cur)))
    ttt.set_status_item("left", "ask", "forseti: open a file in the repo first")
    ttt.set_timeout(3500, function() ttt.remove_status_item("ask") end)
    return
  end
  local root = cur:match("(.*/repos/[^/]+)")
  if not root and CREW_REPO then root = CREW_REPO end
  local ok_s, stat = pcall(sys.exec, "git", { "-C", root, "status", "--porcelain" }, "")
  if not ok_s or not stat or (stat.exit_code or 0) ~= 0 then
    log_err("ask_diff: git status failed (" .. tostring(ok_s) .. ")")
    ttt.set_status_item("left", "ask", "forseti: git status failed")
    ttt.set_timeout(3000, function() ttt.remove_status_item("ask") end)
    return
  end
  if stat.stdout == "" then
    ttt.set_status_item("left", "ask", "forseti: working tree clean — nothing to ask about")
    ttt.set_timeout(3000, function() ttt.remove_status_item("ask") end)
    return
  end
  local ok_d, diff = pcall(sys.exec, "git", { "-C", root, "diff" }, "")
  local excerpt = (ok_d and diff and diff.stdout) or ""
  if #excerpt > 2000 then excerpt = excerpt:sub(1, 2000) .. "\n… (truncated)" end
  local question = "Explain these uncommitted changes:\n\n"
    .. "```\n" .. stat.stdout .. "```\n"
    .. (excerpt ~= "" and ("```diff\n" .. excerpt .. "\n```\n") or "")
  local ok_q = herdr_cmd({ "agent", "prompt", "coder", question })
  ttt.set_status_item("left", "ask", ok_q and "asked pi about uncommitted changes" or "forseti: prompt failed")
  ttt.set_timeout(3000, function() ttt.remove_status_item("ask") end)
end


ttt.register({
  sidebar = { title = "Forseti", render = render },
  commands = {
    { id = "forseti.jump", title = "Forseti: Jump", handler = jump },
    { id = "forseti.ask", title = "Forseti: Ask pi about selection", handler = quick_ask },
    { id = "forseti.review", title = "Forseti: Review", handler = review },
    { id = "forseti.git", title = "Forseti: Git (lazygit pane)", handler = open_git },
    { id = "forseti.ask_diff", title = "Forseti: Ask pi about uncommitted changes", handler = ask_diff },
    { id = "forseti.daily", title = "Forseti: Daily Note", handler = daily_note },
    { id = "forseti.backlinks", title = "Forseti: Backlinks", handler = backlinks },
    { id = "forseti.wikilink", title = "Forseti: Follow Wikilink", handler = wikilink_jump },
    { id = "forseti.obsidian", title = "Forseti: Open in Obsidian", handler = open_obsidian },
    {
      id = "forseti.toggle_focus",
      title = "Forseti: Toggle focus pi on ask",
      handler = function()
        auto_focus = not auto_focus
        ttt.set_status_item("left", "ask", "focus-on-ask " .. (auto_focus and "ON" or "OFF"))
        ttt.set_timeout(2500, function() ttt.remove_status_item("ask") end)
      end,
    },
  },
  keybindings = {
    { key = "ctrl+k a", command = "forseti.ask" },
  },
})