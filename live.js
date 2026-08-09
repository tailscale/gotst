(function() {
    "use strict";

    var currentHTML = "";
    var ws = null;
    var finished = false;

    function morph(live, fresh) {
        var liveChildren = live.childNodes;
        var freshChildren = fresh.childNodes;
        var i = 0;
        for (; i < freshChildren.length; i++) {
            if (i >= liveChildren.length) {
                live.appendChild(freshChildren[i].cloneNode(true));
                continue;
            }
            var oldNode = liveChildren[i];
            var newNode = freshChildren[i];
            if (oldNode.nodeType === 1 && newNode.nodeType === 1 && oldNode.tagName === newNode.tagName) {
                morphAttrs(oldNode, newNode);
                morph(oldNode, newNode);
            } else if (oldNode.nodeType === 3 && newNode.nodeType === 3) {
                if (oldNode.nodeValue !== newNode.nodeValue) oldNode.nodeValue = newNode.nodeValue;
            } else {
                live.replaceChild(newNode.cloneNode(true), oldNode);
            }
        }
        while (liveChildren.length > freshChildren.length) {
            live.removeChild(liveChildren[freshChildren.length]);
        }
    }

    function morphAttrs(live, fresh) {
        for (var i = live.attributes.length - 1; i >= 0; i--) {
            var name = live.attributes[i].name;
            if (!fresh.hasAttribute(name)) live.removeAttribute(name);
        }
        for (var j = 0; j < fresh.attributes.length; j++) {
            var attr = fresh.attributes[j];
            if (live.getAttribute(attr.name) !== attr.value) live.setAttribute(attr.name, attr.value);
        }
    }

    function applyPatch(patch) {
        var oldRunes = Array.from(currentHTML);
        var insertRunes = Array.from(patch.insert || "");
        var suffix = patch.suffix || 0;
        currentHTML = oldRunes.slice(0, patch.prefix)
            .concat(insertRunes)
            .concat(suffix ? oldRunes.slice(oldRunes.length - suffix) : [])
            .join("");
        var fresh = new DOMParser().parseFromString(currentHTML, "text/html");
        morph(document.body, fresh.body);
        if (patch.final) {
            finished = true;
            ws.send(JSON.stringify({ack: patch.seq}));
        }
    }

    function connect() {
        var proto = window.location.protocol === "https:" ? "wss:" : "ws:";
        ws = new WebSocket(proto + "//" + window.location.host + "/live-ws");
        ws.onmessage = function(event) {
            try { applyPatch(JSON.parse(event.data)); } catch (_) {}
        };
        ws.onclose = function() {
            ws = null;
            if (!finished) setTimeout(connect, 1000);
        };
        ws.onerror = function() { ws.close(); };
    }

    connect();
})();
