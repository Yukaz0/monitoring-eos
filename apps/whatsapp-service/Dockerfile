# whatsapp-service monitoring: image node, sesi Baileys di volume.
FROM node:22-alpine

# git dibutuhkan npm untuk dependency dari tarball GitHub (mis. Baileys fork).
RUN apk add --no-cache git

WORKDIR /app

COPY package.json package-lock.json* ./
# Jangan matikan npm audit agar warning tetap terlihat saat build.
RUN npm install --omit=dev

COPY src ./src

ENV NODE_ENV=production
EXPOSE 3101

CMD ["node", "src/index.js"]
