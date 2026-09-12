/* sing-box hub UI glue: theme handling, toasts, modal close, copy buttons. */
(function () {
  'use strict';

  // ---- toast ----
  function showToast(type, text) {
    var stack = document.getElementById('toast-stack');
    if (!stack) return;
    var el = document.createElement('div');
    el.className = 'toast mono toast-' + (type === 'ok' ? 'ok' : 'err');
    el.textContent = text;
    stack.appendChild(el);
    setTimeout(function () { el.remove(); }, 4000);
  }

  document.body.addEventListener('ui-toast', function (evt) {
    var d = evt.detail || {};
    showToast(d.type, d.text);
  });

  document.body.addEventListener('htmx:responseError', function () {
    showToast('err', '请求失败,请重试');
  });

  document.body.addEventListener('htmx:sendError', function () {
    showToast('err', '网络错误,请检查连接');
  });

  // Subscription filters are delegated because the node table is refreshed
  // by htmx every few seconds.
  function applySubscriptionFilter() {
    var filter = document.getElementById('sub-filter');
    if (!filter) return;
    var active = filter.querySelector('button.active') || filter.querySelector('button');
    if (!active) return;
    var group = active.dataset.group;
    document.querySelectorAll('#sub-nodes tbody tr').forEach(function (row) {
      row.style.display = row.dataset.group === group ? '' : 'none';
    });
  }

  document.addEventListener('click', function (evt) {
    var btn = evt.target.closest('#sub-filter button');
    if (!btn) return;
    var filter = btn.closest('#sub-filter');
    filter.querySelectorAll('button').forEach(function (item) {
      item.classList.toggle('active', item === btn);
    });
    applySubscriptionFilter();
  });

  document.body.addEventListener('htmx:afterSwap', function (evt) {
    if (evt.detail.target && (evt.detail.target.id === 'sub-nodes' ||
        document.getElementById('sub-filter'))) {
      applySubscriptionFilter();
    }
  });

  applySubscriptionFilter();

  // ---- modal close with animation ----
  function closeModal(mask) {
    if (!mask) return;
    mask.classList.add('closing');
    setTimeout(function () { mask.remove(); }, 150);
  }

  // ---- delegated clicks (survives htmx swaps) ----
  document.addEventListener('click', function (evt) {
    var t = evt.target;

    // profile edit button - use JS fetch instead of htmx
    if (t.closest('[data-action="open-profile-edit"]')) {
      fetch('/user/profile/edit').then(function(r){ return r.text(); }).then(function(html){
        document.body.insertAdjacentHTML('beforeend', html);
      });
      evt.stopPropagation();
      return;
    }

    // add node from manage modal: close manage modal first, then load add form
    if (t.id === 'btn-add-from-manage' || t.closest('#btn-add-from-manage')) {
      closeModal(document.querySelector('.modal-mask'));
      htmx.ajax('GET', '/admin/nodes/new', { target: '#node-form-slot', swap: 'innerHTML' });
      evt.stopPropagation();
      return;
    }

    // modal close buttons
    var closer = t.closest('.ui-close-modal');
    if (closer) {
      var mask = closer.closest('.modal-mask');
      if (mask) closeModal(mask);
      return;
    }
    // click on the mask itself closes the modal
    if (t.dataset && t.dataset.click === 'close-mask') {
      closeModal(t);
      return;
    }
    if (t.classList && t.classList.contains('modal-mask')) {
      closeModal(t);
      return;
    }
    // copy buttons
    var copyBtn = t.closest('[data-copy]');
    if (copyBtn) {
      var input = document.querySelector(copyBtn.getAttribute('data-copy'));
      if (input && navigator.clipboard) {
        var textToCopy = input.dataset.fullUrl || input.value;
        var originalText = copyBtn.textContent;
        navigator.clipboard.writeText(textToCopy).then(
          function () {
            copyBtn.textContent = '已复制';
            copyBtn.disabled = true;
            setTimeout(function () {
              copyBtn.textContent = originalText;
              copyBtn.disabled = false;
            }, 1500);
            showToast('ok', '已复制');
          },
          function () { showToast('err', '复制失败'); }
        );
      }
    }
  });

  // Profile edit is fetched into the document dynamically, so submit it
  // explicitly instead of relying on htmx to initialize the new form node.
  document.addEventListener('submit', function (evt) {
    var form = evt.target.closest('#profile-edit-form');
    if (!form) return;
    evt.preventDefault();
    evt.stopImmediatePropagation();
    var submit = form.querySelector('button[type="submit"]');
    if (submit) submit.disabled = true;
    fetch(form.action, {
      method: 'POST',
      body: new URLSearchParams(new FormData(form)),
      headers: {'HX-Request': 'true'}
    }).then(function (response) {
      var raw = response.headers.get('HX-Trigger');
      if (raw) {
        try {
          var events = JSON.parse(raw);
          Object.keys(events).forEach(function (name) {
            document.body.dispatchEvent(new CustomEvent(name, {
              bubbles: true,
              detail: events[name]
            }));
          });
        } catch (_) {
          showToast('err', '保存响应异常');
        }
      }
      if (response.ok) {
        var updated = raw && raw.indexOf('profile-updated') !== -1;
        if (updated) closeModal(form.closest('.modal-mask'));
      } else {
        showToast('err', '保存失败');
      }
    }).catch(function () {
      showToast('err', '网络错误,请检查连接');
    }).finally(function () {
      if (submit) submit.disabled = false;
    });
  });

  // ---- brand upload ----
  function doUpload(url, file, fieldName, cb) {
    if (!file) { if (cb) cb(); return; }
    var fd = new FormData();
    fd.append(fieldName, file);
    var xhr = new XMLHttpRequest();
    xhr.open('POST', url);
    xhr.onload = function () {
      if (xhr.status === 200) { if (cb) cb(); }
    };
    xhr.send(fd);
  }

  document.addEventListener('click', function (evt) {
    var btn = evt.target.closest('#btn-save-brand');
    if (!btn) return;
    var form = document.getElementById('brand-form');
    if (!form) return;
    var fd = new FormData(form);
    var faviconInput = document.getElementById('favicon-input');
    var logoInput = document.getElementById('logo-input');
    if (faviconInput && faviconInput.files && faviconInput.files[0]) {
      fd.append('favicon', faviconInput.files[0]);
    }
    if (logoInput && logoInput.files && logoInput.files[0]) {
      fd.append('logo', logoInput.files[0]);
    }
    var xhr = new XMLHttpRequest();
    xhr.open('POST', '/admin/branding');
    xhr.setRequestHeader('HX-Request', 'true');
    xhr.onload = function () {
      if (xhr.status === 200) {
        showToast('ok', '品牌设置已保存');
        // Apply main content swap
        var card = document.getElementById('settings-card');
        if (card) {
          var tmp = document.createElement('div');
          tmp.innerHTML = xhr.responseText;
          var settingsCard = tmp.querySelector('#settings-card');
          if (settingsCard) {
            card.innerHTML = settingsCard.innerHTML;
          }
        }
        // Apply OOB sidebar brand swap
        var tmp2 = document.createElement('div');
        tmp2.innerHTML = xhr.responseText;
        var oobBrand = tmp2.querySelector('.sidebar-brand[hx-swap-oob]');
        if (oobBrand) {
          var existing = document.querySelector('.sidebar-brand');
          if (existing) {
            existing.innerHTML = oobBrand.innerHTML;
          }
          var brandText = oobBrand.querySelector('.brand-text');
          if (brandText) {
            var name = brandText.textContent.trim() || 'sing-box hub';
            document.title = document.title.replace(/·\s*sing-box hub/, '· ' + name);
          }
        }
      } else {
        showToast('err', '保存失败');
      }
    };
    xhr.onerror = function () { showToast('err', '网络错误'); };
    xhr.send(fd);
  });

  // brand file preview
  document.addEventListener('change', function (evt) {
    var inp = evt.target;
    if (inp.id !== 'favicon-input' && inp.id !== 'logo-input') return;
    var box = inp.closest('.brand-box');
    if (!box || !inp.files || !inp.files[0]) return;
    var reader = new FileReader();
    reader.onload = function (e) {
      var old = box.querySelector('img, .brand-placeholder');
      if (old) old.remove();
      var img = document.createElement('img');
      img.src = e.target.result;
      box.appendChild(img);
    };
    reader.readAsDataURL(inp.files[0]);
  });

  // ---- HX-Trigger events from the Go side ----
  // htmx fires custom events named after the keys in HX-Trigger; the ui-toast
  // listener above receives {type,text} as evt.detail.

  // ---- profile reset confirm ----
  document.body.addEventListener('click', function (evt) {
    var btn = evt.target.closest('#btn-confirm-reset');
    if (btn) {
      var mask = btn.closest('.modal-mask');
      if (mask) closeModal(mask);
    }
  });

  // ---- close edit modal after successful save ----
  document.body.addEventListener('htmx:afterRequest', function (evt) {
    if (evt.detail.pathInfo && evt.detail.pathInfo.requestPath === '/user/profile/edit') {
      var editModal = document.getElementById('profile-edit-modal');
      if (editModal && evt.detail.successful) {
        closeModal(editModal.closest('.modal-mask'));
      }
    }
    // After subscription token reset, update only the subscription section
    if (evt.detail.pathInfo && evt.detail.pathInfo.requestPath === '/user/subscription/token') {
      if (evt.detail.successful) {
        var existingSubSection = document.getElementById('profile-sub-section');
        if (existingSubSection) {
          fetch('/user/profile').then(function(r){ return r.text(); }).then(function(html){
            var tmp = document.createElement('div');
            tmp.innerHTML = html;
            var newSubSection = tmp.querySelector('#profile-sub-section');
            if (newSubSection) {
              existingSubSection.replaceWith(newSubSection);
            }
          });
        }
      }
    }
  });

  // The profile response rotates the session after a username/password
  // change. Refresh the original profile modal with the new claims.
  document.body.addEventListener('profile-updated', function () {
    fetch('/user/profile').then(function (r) { return r.text(); }).then(function (html) {
      var tmp = document.createElement('div');
      tmp.innerHTML = html;
      var next = tmp.querySelector('#profile-modal');
      var current = document.getElementById('profile-modal');
      if (next && current) current.replaceWith(next);
    });
  });

  // ---- sync tab title after OOB brand swap ----
  document.body.addEventListener('htmx:oobAfterSwap', function (evt) {
    if (evt.detail.target && evt.detail.target.classList &&
        evt.detail.target.classList.contains('sidebar-brand')) {
      var brandText = evt.detail.target.querySelector('.brand-text');
      if (brandText) {
        var name = brandText.textContent.trim() || 'sing-box hub';
        document.title = document.title.replace(/·\s*sing-box hub/, '· ' + name);
      }
    }
  });

  // ---- sync tab title after settings save (htmx swap) ----
  document.body.addEventListener('htmx:afterSwap', function (evt) {
    if (evt.detail.target && evt.detail.target.id === 'settings-card') {
      // Check for OOB sidebar brand in the response
      var oobBrand = document.querySelector('.sidebar-brand');
      if (oobBrand) {
        var brandText = oobBrand.querySelector('.brand-text');
        if (brandText) {
          var name = brandText.textContent.trim() || 'sing-box hub';
          document.title = document.title.replace(/·\s*sing-box hub/, '· ' + name);
        }
      }
    }
  });
})();
