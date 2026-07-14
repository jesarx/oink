# oink 🐷

Rastreador de gastos personal, minimalista, mobile-first, para un solo usuario. Un solo binario en Go (stdlib + `lib/pq`), plantillas y estáticos embebidos, PostgreSQL, PWA con tema oscuro.

## La lógica

Todo gira alrededor del **disponible del mes**:

```
Disponible = Entradas recibidas − Meta de ahorro − Gastos fijos (pagados y pendientes) − Tarjeta − Retiros
```

- La **meta de ahorro** (default $15,000) se aparta desde el día uno: ese dinero no existe.
- Los **gastos fijos** se descuentan completos aunque no estén pagados; marcarlos como pagados solo registra la fecha. Un fijo puede marcarse como **"se paga en efectivo"**: al pagarlo se descuenta del sobre semanal y no del disponible bancario (el retiro que fondeó ese efectivo ya contó).
- La **tarjeta de crédito cuenta cuando gastas, no cuando la pagas**: cada compra a crédito descuenta del disponible al instante (ese mes consumiste ese dinero). El **pago de la tarjeta** se registra con "Pagué la tarjeta" en el home y **no cuenta como gasto** —solo baja la deuda acumulada—, porque ya contó el mes en que compraste.
- El **sobre semanal de efectivo**: el retiro del cajero descuenta del disponible; los gastos en efectivo solo descuentan del sobre (el dinero ya salió al retirarlo). El sobre corre de lunes a domingo.
- Las **entradas en efectivo** (no fijas marcadas "en efectivo") van directo al sobre semanal: cuentan como entrada del mes, pero no engordan el disponible bancario, porque ese dinero nunca pasó por el banco.
- El **ciclo mensual no es el mes calendario**: empieza cuando registras la primera de tus entradas. Si registras una entrada que ya llegó en el ciclo abierto, oink cierra el mes y abre uno nuevo.
- Los **préstamos** (lo que prestas y te prestan) no son gasto ni entrada: son transferencias. Si eliges reflejarlos, mueven el sobre (efectivo) o el disponible (tarjeta/banco) al registrarlos y al saldarlos, pero nunca ensucian los reportes de consumo.
- **Funciona sin conexión**: la app abre con la última copia vista de cada página, y los gastos y pendientes capturados sin red se guardan en una cola local que se sincroniza sola al volver la conexión (cada registro lleva un id único, así que un reintento nunca duplica).
- **Pendientes**: una lista simple de tareas (solo texto, sin título) con marcar/desmarcar como hecho, también utilizable offline.

## Requisitos

- Go 1.22+ (solo para compilar; en producción corre el binario)
- PostgreSQL
- nginx + certbot

## Despliegue en el VPS

### 1. Base de datos

```bash
sudo -u postgres psql
CREATE USER oink_user WITH PASSWORD 'un_password_seguro';
CREATE DATABASE oink OWNER oink_user;
\q
```

El esquema se crea solo al arrancar (migración automática con `IF NOT EXISTS`).

### 2. Compilar

```bash
cd /var/www
sudo git clone <tu-repo>/oink.git && cd oink
go build -o oink .
```

(O compila en tu ThinkPad con `GOOS=linux GOARCH=amd64 go build -o oink .` y sube solo el binario con scp; todo va embebido.)

### 3. Contraseña y entorno

```bash
./oink hash          # escribe tu contraseña, copia el hash resultante
sudo vim /etc/oink.env
```

Contenido de `/etc/oink.env`:

```
OINK_DSN=postgres://oink_user:un_password_seguro@localhost/oink?sslmode=disable
OINK_PASSWORD_HASH=pbkdf2-sha256$300000$...$...
OINK_ADDR=127.0.0.1:4100
OINK_TZ=America/Mexico_City
```

```bash
sudo chmod 600 /etc/oink.env
```

### 4. systemd y nginx

```bash
sudo cp deploy/oink.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now oink
sudo journalctl -u oink -n 20   # debe decir "oink escuchando en 127.0.0.1:4100"

sudo cp deploy/nginx-oink.conf /etc/nginx/sites-available/oink
sudo ln -s /etc/nginx/sites-available/oink /etc/nginx/sites-enabled/
sudo nginx -t && sudo systemctl reload nginx
sudo certbot --nginx -d oink.qumran.cc
```

No olvides el registro DNS `A` para `oink.qumran.cc` apuntando al VPS.

### 5. En el celular

Abre `https://oink.qumran.cc`, entra con tu contraseña, y en el menú del navegador elige **"Agregar a pantalla de inicio"**. Queda instalada como app con el cochinito de ícono. La sesión dura 30 días.

## Primer uso

1. **Entradas** → da de alta tus nóminas (nombre y monto habitual).
2. **Fijos** → da de alta renta, internet, suscripciones.
3. **Ajustes** → confirma meta de ahorro ($15,000) y retiro semanal ($1,000).
4. Cuando caiga tu primer pago: **Entradas → "Ya llegó"**. Ahí arranca tu mes.
5. Registra cada gasto desde el home: dos taps (monto + guardar); el concepto es opcional.

## Seguridad

- Sin npm ni Node en producción; una sola dependencia externa (`lib/pq`, driver puro).
- Contraseña con PBKDF2-HMAC-SHA256 (300,000 iteraciones), comparación en tiempo constante.
- Sesiones opacas en Postgres, cookie `Secure` + `HttpOnly` + `SameSite=Strict`.
- Rate limiting: 5 intentos fallidos → bloqueo de 15 minutos.
- Verificación de `Origin` en todos los POST (defensa CSRF adicional).
- CSP estricta, `X-Frame-Options: DENY`, `nosniff`.
- systemd con sandbox (`ProtectSystem=strict`, `NoNewPrivileges`, etc.).

## Backup

Agrega a tu script de respaldos existente:

```bash
pg_dump -Fc -U oink_user oink > backup_oink.dump
```

## Actualizar

```bash
cd /var/www/oink && git pull && go build -o oink . && sudo systemctl restart oink
```
