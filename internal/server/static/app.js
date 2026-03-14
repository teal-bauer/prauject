(function() {
    let term, fitAddon, ws;

    const TERM_FONT = "'JetBrains Mono', 'Cascadia Code', 'Fira Code', Consolas, 'SF Mono', 'Ubuntu Mono', 'DejaVu Sans Mono', Menlo, monospace";

    function sendResize() {
        if (term && ws && ws.readyState === WebSocket.OPEN) {
            ws.send(JSON.stringify({type: 'resize', data: term.cols + 'x' + term.rows}));
        }
    }

    function initTerminal() {
        const container = document.getElementById('xterm-container');
        if (!container) return;
        if (term) { term.dispose(); term = null; }

        term = new window.Terminal({
            theme: {
                background: '#0a0a0a',
                foreground: '#e0e0e0',
                cursor: '#22c55e',
                selectionBackground: '#22c55e33',
            },
            fontFamily: TERM_FONT,
            fontSize: 11,
            cursorBlink: true,
            convertEol: true,
            scrollback: 5000,
        });
        fitAddon = new window.FitAddon.FitAddon();
        term.loadAddon(fitAddon);
        term.open(container);

        const sessionId = container.dataset.sessionId;
        connectWebSocket(sessionId);

        term.onData(function(data) {
            if (ws && ws.readyState === WebSocket.OPEN) {
                ws.send(JSON.stringify({type: 'input', data: data}));
            }
        });
    }

    function connectWebSocket(sessionId) {
        if (ws) { ws.close(); ws = null; }
        const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
        ws = new WebSocket(proto + '//' + location.host + '/sessions/' + sessionId + '/ws');

        ws.onmessage = function(e) {
            const msg = JSON.parse(e.data);
            if (msg.type === 'size') {
                // Server tells us tmux pane size, e.g. "155x72"
                const parts = msg.data.split('x');
                const cols = parseInt(parts[0], 10);
                const rows = parseInt(parts[1], 10);
                if (cols > 0 && rows > 0) {
                    term.resize(cols, rows);
                }
            } else if (msg.type === 'terminal') {
                term.clear();
                term.write(msg.data);
            }
        };

        ws.onopen = function() {};

        ws.onclose = function() {
            if (!term) return;
            term.write('\r\n\x1b[33m[disconnected — reconnecting...]\x1b[0m\r\n');
            setTimeout(() => {
                if (term && document.getElementById('xterm-container')) {
                    connectWebSocket(sessionId);
                }
            }, 2000);
        };

        ws.onerror = function() {};
    }

    function cleanupTerminal() {
        if (ws) { ws.close(); ws = null; }
        if (term) { term.dispose(); term = null; }
        fitAddon = null;
    }

    // Handle detail pane updates
    document.body.addEventListener('htmx:beforeSwap', function(e) {
        if (e.detail.target.id === 'detail-pane') {
            cleanupTerminal();
        }
    });

    document.body.addEventListener('htmx:afterSwap', function(e) {
        if (e.detail.target.id === 'detail-pane') {
            cleanupTerminal();
            requestAnimationFrame(() => {
                const scroll = document.getElementById('detail-scroll');
                if (scroll) scroll.scrollTop = scroll.scrollHeight;
            });
        }
    });

    // Highlight active session card
    document.body.addEventListener('htmx:afterOnLoad', function(e) {
        if (e.detail.target.id === 'detail-pane') {
            document.querySelectorAll('.session-card').forEach(c => c.classList.remove('active'));
            if (e.detail.elt && e.detail.elt.classList.contains('session-card')) {
                e.detail.elt.classList.add('active');
            }
        }
    });

    // Get the session ID and message count from the detail pane
    function getDetailState() {
        const el = document.getElementById('msg-count');
        if (!el) return null;
        return {
            sessionId: el.dataset.session,
            count: parseInt(el.dataset.count, 10),
        };
    }

    // Check if detail pane is scrolled to the bottom
    function isScrolledToBottom(el) {
        return el.scrollHeight - el.scrollTop - el.clientHeight < 50;
    }

    // Append new messages to the chat log
    function appendMessages(sessionId, currentCount) {
        const chatLog = document.querySelector('.chat-log');
        const scrollPane = document.getElementById('detail-scroll');
        if (!chatLog || !scrollPane) return;

        const wasAtBottom = isScrolledToBottom(scrollPane);

        fetch('/sessions/' + sessionId + '/tail?after=' + currentCount)
            .then(r => r.text())
            .then(html => {
                html = html.trim();
                if (!html || html.startsWith('<!--')) return;

                // Append the new messages before the msg-count marker
                const marker = document.getElementById('msg-count');
                const temp = document.createElement('div');
                temp.innerHTML = html;

                // Update the count from the response
                const newMarker = temp.querySelector('#msg-count');
                if (newMarker) {
                    marker.dataset.count = newMarker.dataset.count;
                    newMarker.remove();
                }

                // Append each new message element
                while (temp.firstChild) {
                    chatLog.appendChild(temp.firstChild);
                }

                // Scroll to bottom if we were already there
                if (wasAtBottom) {
                    scrollPane.scrollTop = scrollPane.scrollHeight;
                }
            });
    }

    // SSE for live updates
    const evtSource = new EventSource('/events');
    evtSource.addEventListener('update', function(e) {
        const data = e.data;

        if (data === 'sessions-updated' || data === 'loading-complete') {
            const list = document.getElementById('session-list');
            if (list) {
                htmx.ajax('GET', '/?sort=recent', {target: '#session-list', swap: 'innerHTML'});
            }
        }

        if (data.startsWith('session-updated:')) {
            const updatedId = data.split(':')[1];
            const state = getDetailState();
            if (state && state.sessionId === updatedId) {
                appendMessages(updatedId, state.count);
            }
            // Also refresh the session list to update message counts etc
            const list = document.getElementById('session-list');
            if (list) {
                htmx.ajax('GET', '/?sort=recent', {target: '#session-list', swap: 'innerHTML'});
            }
        }
    });

    // Scroll chat to bottom if a session is already open
    const scroll = document.getElementById('detail-scroll');
    if (scroll) {
        scroll.scrollTop = scroll.scrollHeight;
    }

    // Toggle terminal overlay
    window.toggleTerminal = function() {
        const overlay = document.getElementById('xterm-overlay');
        if (!overlay) return;
        const visible = overlay.style.display !== 'none';
        overlay.style.display = visible ? 'none' : 'flex';
        if (!visible) {
            if (!term) {
                initTerminal();
            }
            // Fit and resize after layout settles
            setTimeout(() => {
                if (fitAddon) fitAddon.fit();
                sendResize();
            }, 150);
        } else {
            // Closing — tell server to restore tmux sizing
            if (ws && ws.readyState === WebSocket.OPEN) {
                ws.send(JSON.stringify({type: 'resize-restore'}));
            }
        }
    };


    // Copy attach command
    window.copyAttachCmd = function(btn) {
        const cmd = btn.dataset.cmd;
        navigator.clipboard.writeText(cmd).then(() => {
            btn.textContent = 'copied!';
            setTimeout(() => { btn.textContent = 'copy'; }, 2000);
        });
    };
})();
