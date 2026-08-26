// 登录页错误提示：?error=1 时显示
(function () {
  var p = new URLSearchParams(location.search);
  if (p.get('error')) {
    document.getElementById('err').textContent = '令牌错误，请重试';
  }
})();
