(function () {
  'use strict';
  var form = document.querySelector('.setup-form');
  if (!form) return;
  var input = form.querySelector('#dsn');
  var defaultSQLite = input ? input.value : '';
  form.querySelectorAll('input[name="driver"]').forEach(function (radio) {
    radio.addEventListener('change', function () {
      if (!input) return;
      if (radio.value === 'postgres' && radio.checked) {
        if (input.value === defaultSQLite) input.value = '';
        input.placeholder = 'postgres://user:password@host:5432/singhub?sslmode=require';
      }
      if (radio.value === 'sqlite' && radio.checked) {
        if (!input.value) input.value = defaultSQLite;
        input.placeholder = 'data/panel.db';
      }
    });
  });
}());
